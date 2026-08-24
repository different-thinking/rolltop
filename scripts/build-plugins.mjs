// File overview: Builds every frontend plugin bundle, several at a time.
//
// The serial `&&` chain this replaced ran thirteen Vite builds back to back.
// Eight of them transform ~4,550 modules each because a plugin bundle pulls in
// the shared React tree, so the chain was ~50s of wall clock spent almost
// entirely waiting on one core while the rest of the machine idled.
//
// The target list is derived from `plugins/*/manifest.json`, not held here, for
// the same reason the `Dockerfile` derives its backend list from
// `plugins/*/backend`: the hand-maintained copies had already drifted apart. A
// plugin is a frontend build target exactly when its manifest declares
// `frontend.module` — which is also the thing the server asks for at runtime,
// so a plugin cannot be built and then not served, or the reverse.

import { spawn } from "node:child_process";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { availableParallelism, totalmem } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = fileURLToPath(new URL("..", import.meta.url));
const pluginsRoot = path.join(repoRoot, "plugins");

// Stylesheets are compiled by sass, not Vite, and the commands live in
// `package.json` so that `npm run build:themes` keeps working on its own — the
// Go suite's manifest validation stats the compiled theme CSS, so that script
// is a documented prerequisite for `go test`, not just a build step.
//
// A plugin's CSS step must run *after* its Vite build: `emptyOutDir` wipes the
// output directory both write into. Within a plugin the two are therefore
// sequential; it is across plugins that there is nothing to serialize.
//
// This map is the one place a new stylesheet has to be registered. Forgetting
// is not silent: the manifest check at the end of this file fails the build on
// any declared CSS file that no step produced.
const cssScripts = {
  client_side_pgp: "build:pgp-css",
  experimental_spam_filter: "build:experimental-spam-filter-css",
  mail_filters: "build:mail-filters-css",
  matrix_theme: "build:themes",
  remote_imap_sync: "build:remote-imap-sync-css"
};

function readManifests() {
  return readdirSync(pluginsRoot, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => ({
      id: entry.name,
      file: path.join(pluginsRoot, entry.name, "manifest.json")
    }))
    .filter((plugin) => existsSync(plugin.file))
    .map((plugin) => ({
      ...plugin,
      manifest: JSON.parse(readFileSync(plugin.file, "utf8"))
    }));
}

// `env` is passed per call rather than set on `process.env`, because several of
// these run at once and they would otherwise be writing one plugin's target
// into the environment the next one is about to read.
function run(command, args, label, env = process.env) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: repoRoot,
      // Inherited so a failing build prints its own diagnostics. Interleaving
      // is the price of running several at once; the plugin that failed is
      // named in the rejection below.
      stdio: "inherit",
      env
    });
    child.on("error", reject);
    child.on("close", (code, signal) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`${label} failed (${signal ? `signal ${signal}` : `exit ${code}`})`));
    });
  });
}

// Spawned through `process.execPath` rather than the `vite` shim so the child
// does not depend on `node_modules/.bin` being on PATH.
const viteBin = path.join(repoRoot, "node_modules", "vite", "bin", "vite.js");
const npmBin = process.env.npm_execpath;

async function buildPlugin({ id }) {
  await run(
    process.execPath,
    [viteBin, "build", "--config", "vite.plugins.config.ts"],
    `plugin ${id}: vite build`,
    { ...process.env, ROLLTOP_PLUGIN_TARGET: id }
  );

  const script = cssScripts[id];
  if (!script) {
    return;
  }
  // `npm_execpath` is set whenever this runs under `npm run`. Falling back to
  // the `npm` on PATH keeps a direct `node scripts/build-plugins.mjs` working.
  if (npmBin) {
    await run(process.execPath, [npmBin, "run", script], `plugin ${id}: ${script}`);
  } else {
    await run("npm", ["run", script], `plugin ${id}: ${script}`);
  }
}

// `os.totalmem()` reports the host's memory, not the container's, so a build
// box with a small limit would otherwise look enormous and be told to run four
// Node processes inside a budget for one. The cgroup limit is the number that
// actually gets enforced, and exceeding it is not a slow build but a killed one.
function memoryLimitBytes() {
  for (const file of ["/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"]) {
    try {
      const raw = readFileSync(file, "utf8").trim();
      if (!raw || raw === "max") continue;
      const value = Number(raw);
      // cgroup v1 spells "unlimited" as a number near 2^63.
      if (Number.isFinite(value) && value > 0 && value < Number.MAX_SAFE_INTEGER) {
        return value;
      }
    } catch {
      // No cgroup files: not a container, or a kernel that puts them elsewhere.
    }
  }
  return totalmem();
}

// Each Vite build is a separate Node process holding a full module graph, so
// concurrency is bounded by memory rather than by cores. Going wide on a
// memory-capped builder does not slow the build down, it gets the build OOM
// killed — which is exactly what was happening on ours, where three killed
// attempts read as one thirteen-minute build. Budget ~1.5 GB per job and let
// `ROLLTOP_BUILD_JOBS` override when the shape of the machine is known.
const bytesPerJob = 1.5 * 1024 * 1024 * 1024;

function jobLimit() {
  const requested = Number.parseInt(process.env.ROLLTOP_BUILD_JOBS ?? "", 10);
  if (Number.isInteger(requested) && requested > 0) {
    return requested;
  }
  const byMemory = Math.floor(memoryLimitBytes() / bytesPerJob);
  return Math.max(1, Math.min(4, availableParallelism(), byMemory));
}

async function runPool(items, limit, worker) {
  let next = 0;
  const failures = [];
  const runners = Array.from({ length: Math.min(limit, items.length) }, async () => {
    for (;;) {
      const index = next++;
      if (index >= items.length) {
        return;
      }
      try {
        await worker(items[index]);
      } catch (error) {
        // Collected rather than thrown so one broken plugin does not hide the
        // state of the others; the build still fails below.
        failures.push(error);
      }
    }
  });
  await Promise.all(runners);
  return failures;
}

// The manifest is what the server reads, so it is also what proves the build
// produced everything it was supposed to. Without this a plugin whose CSS step
// was never registered above would ship a manifest pointing at a file that does
// not exist — and for a theme that is not a cosmetic problem: the manifest
// loader stats theme CSS at startup and refuses to boot when it is missing.
function missingDeclaredFiles(plugins) {
  const missing = [];
  for (const { id, manifest } of plugins) {
    const declared = [
      manifest.frontend?.module,
      manifest.frontend?.css,
      ...(manifest.themes ?? []).map((theme) => theme.css)
    ].filter(Boolean);
    for (const relative of declared) {
      const absolute = path.join(pluginsRoot, id, relative);
      if (!existsSync(absolute)) {
        missing.push(`${id}: ${relative}`);
      }
    }
  }
  return missing;
}

const plugins = readManifests();
const targets = plugins.filter((plugin) => plugin.manifest.frontend?.module);
const limit = jobLimit();

console.log(`Building ${targets.length} frontend plugins, ${limit} at a time`);

const failures = await runPool(targets, limit, buildPlugin);

if (failures.length > 0) {
  for (const failure of failures) {
    console.error(failure.message);
  }
  process.exit(1);
}

const missing = missingDeclaredFiles(plugins);
if (missing.length > 0) {
  console.error("Manifest declares files the build did not produce:");
  for (const entry of missing) {
    console.error(`  ${entry}`);
  }
  console.error("A new stylesheet needs an entry in `cssScripts` in this file.");
  process.exit(1);
}
