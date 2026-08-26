// File overview: Copies the runtime half of `plugins/` into a deployable tree.
//
// The image used to take the whole plugin source tree: `.tsx` and `.go`
// sources, `.scss` inputs, training data, READMEs and the sourcemaps beside
// every bundle. None of it is reachable at runtime — the asset route serves
// only the directories a manifest declares (`pluginAssetAllowed` in
// `backend/web/plugin_routes.go`) — but all of it was snapshotted into a layer
// and pushed to a registry on every build.
//
// What the server actually reads under a plugin directory is a short list, and
// this derives it from the manifest rather than restating it:
//
//   manifest.json          the file that makes the directory a plugin at all
//   frontend.module        served as an asset; its whole directory goes
//   frontend.css           same, and it is not always the same directory —
//                          `client_side_pgp` declares `frontend/dist/index.js`
//                          against `frontend_dist/styles/pgp.css`
//   themes[].css           same, and stat'd at startup: a missing theme CSS
//                          fails `LoadManifests` and the process does not boot
//   migrations/            read at startup and applied to the tenant schema
//
// `backend.binary` is deliberately absent: the `.so` files come from the Go
// stage, which is the only stage that has them.

import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { declaredManifestFiles } from "./plugin-manifest.mjs";

const repoRoot = fileURLToPath(new URL("..", import.meta.url));
const pluginsRoot = path.join(repoRoot, "plugins");

const outRoot = process.argv[2];
if (!outRoot) {
  console.error("usage: node scripts/assemble-plugin-dist.mjs <output-dir>");
  process.exit(1);
}

// A manifest path is served as "everything under the declared file's own
// directory", because a bundle sits next to the chunks and assets it imports.
// Mirroring that here is what keeps a code-split plugin working.
function parentDirectory(relative) {
  const parent = path.posix.dirname(relative);
  if (parent === "." || parent === "..") {
    // A manifest pointing at a bare filename would mean "copy the plugin
    // root", which is the thing this script exists to avoid. No manifest does
    // it today; say so rather than silently copying everything.
    throw new Error(`refusing to copy a whole plugin directory for ${relative}`);
  }
  return parent;
}

let copied = 0;
const missing = [];

for (const entry of readdirSync(pluginsRoot, { withFileTypes: true })) {
  if (!entry.isDirectory()) {
    continue;
  }
  const manifestPath = path.join(pluginsRoot, entry.name, "manifest.json");
  if (!existsSync(manifestPath)) {
    // `plugins/bundled` and `plugins/catalog` are Go packages compiled into the
    // main binary. The manifest loader skips them for the same reason.
    continue;
  }

  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  const target = path.join(outRoot, entry.name);
  mkdirSync(target, { recursive: true });
  cpSync(manifestPath, path.join(target, "manifest.json"));

  // Checked as files, not as the directories they live in. An empty
  // `frontend_dist/themes/matrix/` satisfies a directory test and still fails
  // `LoadManifests` at startup, which stats the theme CSS itself and refuses
  // to boot without it — so the file is what has to be proven present.
  const wanted = new Set();
  for (const relative of declaredManifestFiles(manifest)) {
    if (existsSync(path.join(pluginsRoot, entry.name, relative))) {
      wanted.add(parentDirectory(relative));
    } else {
      missing.push(`${entry.name}: ${relative}`);
    }
  }
  if (existsSync(path.join(pluginsRoot, entry.name, "migrations"))) {
    wanted.add("migrations");
  }

  // A manifest can declare two files whose directories nest — `matrix_theme`
  // names both `frontend_dist/index.js` and
  // `frontend_dist/themes/matrix/theme.css`, so `frontend_dist` and the theme
  // directory inside it both end up wanted, and the inner one would be copied
  // once as part of its parent and once again on its own. Keep only the
  // directories that no other wanted directory already contains.
  const all = Array.from(wanted);
  const outermost = all.filter(
    (relative) => !all.some((other) => other !== relative && relative.startsWith(`${other}/`))
  );

  for (const relative of outermost) {
    const source = path.join(pluginsRoot, entry.name, relative);
    if (!existsSync(source)) {
      missing.push(`${entry.name}: ${relative}`);
      continue;
    }
    const destination = path.join(target, relative);
    mkdirSync(path.dirname(destination), { recursive: true });
    // Sourcemaps are switched off for the image build, but a tree assembled
    // from a developer's working copy may still have them and they are never
    // served on purpose.
    cpSync(source, destination, {
      recursive: true,
      filter: (from) => !from.endsWith(".map")
    });
  }
  copied += 1;
}

if (missing.length > 0) {
  console.error("Manifest declares paths that do not exist:");
  for (const entry of missing) {
    console.error(`  ${entry}`);
  }
  console.error("Run the frontend build before assembling the plugin tree.");
  process.exit(1);
}

console.log(`Assembled ${copied} plugin directories into ${outRoot}`);
