// File overview: Vite library builds for runtime-loaded frontend plugin bundles.

import path from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

const fromRoot = (relative: string) => fileURLToPath(new URL(relative, import.meta.url));

// Redirects the host's icon module to the shim, whatever spelling reaches it.
//
// This was a pattern on the import specifier first, and that was the wrong
// place to look: `frontend/src/components/common.tsx` imports its neighbour as
// `"./Icon"`, which no rule anchored on `components/Icon` can match, so a
// plugin pulling in `common.tsx` would have quietly bundled all 4,543 Phosphor
// modules again. Matching the *resolved file* has no spellings to enumerate —
// every route to that module ends at the same path.
function iconShimPlugin(): Plugin {
  const iconModule = fromRoot("./frontend/src/components/Icon.tsx");
  const shim = fromRoot("./frontend/src/plugins/shared/iconShim.ts");
  return {
    name: "rolltop:icon-shim",
    enforce: "pre",
    async resolveId(source, importer, options) {
      // The shim imports React and the icon runtime, never the icon module, so
      // skipping it here costs nothing and removes any chance of a cycle.
      if (!importer || importer === shim) return null;
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      if (!resolved) return null;
      // A resolved id can carry a `?import`-style suffix, and comparing one of
      // those against a plain path fails quietly — which here would mean the
      // whole Phosphor barrel back in the bundle with the redirect apparently
      // in place. Same silent-bypass shape as the specifier pattern this
      // replaced, so it is worth not relying on the suffix never appearing.
      const file = resolved.id.split("?")[0].split("#")[0];
      return path.resolve(file) === iconModule ? shim : null;
    }
  };
}

const target = (process.env.ROLLTOP_PLUGIN_TARGET || "client_side_pgp").trim();
// `attachment_preview` is deliberately absent. Its UI is part of the
// application bundle — `ThreadView` imports `AttachmentPreviewSlot` directly
// and the slot gates itself on the plugin being enabled — so the runtime bundle
// it used to emit was loaded by every user, registered, and never read by
// anything. It carried PDFium as a 6 MB base64 data URI. See the note in
// AGENTS.md before adding a `frontend` block back to its manifest.
const pluginConfig: Record<string, { entry: string; outDir: string }> = {
  client_side_pgp: {
    entry: "plugins/client_side_pgp/frontend/index.ts",
    outDir: "plugins/client_side_pgp/frontend/dist"
  },
  gravatar_sender_icons: {
    entry: "plugins/gravatar_sender_icons/frontend/index.ts",
    outDir: "plugins/gravatar_sender_icons/frontend_dist"
  },
  bimi_brand_icons: {
    entry: "plugins/bimi_brand_icons/frontend/index.ts",
    outDir: "plugins/bimi_brand_icons/frontend_dist"
  },
  language_search: {
    entry: "plugins/language_search/frontend/index.ts",
    outDir: "plugins/language_search/frontend_dist"
  },
  one_click_unsubscribe: {
    entry: "plugins/one_click_unsubscribe/frontend/index.tsx",
    outDir: "plugins/one_click_unsubscribe/frontend_dist"
  },
  remote_image_blocklist: {
    entry: "plugins/remote_image_blocklist/frontend/index.tsx",
    outDir: "plugins/remote_image_blocklist/frontend_dist"
  },
  trusted_image_sources: {
    entry: "plugins/trusted_image_sources/frontend/index.tsx",
    outDir: "plugins/trusted_image_sources/frontend_dist"
  },
  matrix_theme: {
    entry: "plugins/matrix_theme/frontend/index.ts",
    outDir: "plugins/matrix_theme/frontend_dist"
  },
  mail_filters: {
    entry: "plugins/mail_filters/frontend/index.tsx",
    outDir: "plugins/mail_filters/frontend_dist"
  },
  mail_mcp: {
    entry: "plugins/mail_mcp/frontend/index.tsx",
    outDir: "plugins/mail_mcp/frontend_dist"
  },
  remote_imap_sync: {
    entry: "plugins/remote_imap_sync/frontend/index.tsx",
    outDir: "plugins/remote_imap_sync/frontend_dist"
  },
  experimental_spam_filter: {
    entry: "plugins/experimental_spam_filter/frontend/index.tsx",
    outDir: "plugins/experimental_spam_filter/frontend_dist"
  }
};

const selected = pluginConfig[target];
if (!selected) {
  throw new Error(`Unknown plugin build target: ${target}`);
}

// See `vite.config.ts`: sourcemaps outweigh the bundles they describe, nothing
// serves them, and the image build pays for them twice. Off in the
// `Dockerfile`, on everywhere else.
const sourcemap = process.env.ROLLTOP_BUILD_SOURCEMAPS !== "0";

export default defineConfig({
  root: ".",
  plugins: [iconShimPlugin(), react()],
  define: {
    "process.env.NODE_ENV": JSON.stringify("production")
  },
  resolve: {
    alias: [
      { find: /^react$/, replacement: fromRoot("./frontend/src/plugins/shared/reactShim.ts") },
      { find: /^react-dom$/, replacement: fromRoot("./frontend/src/plugins/shared/reactDOMShim.ts") },
      { find: /^react\/jsx-runtime$/, replacement: fromRoot("./frontend/src/plugins/shared/reactJSXRuntimeShim.ts") },
      { find: /^react\/jsx-dev-runtime$/, replacement: fromRoot("./frontend/src/plugins/shared/reactJSXRuntimeShim.ts") }
    ]
  },
  build: {
    outDir: selected.outDir,
    emptyOutDir: true,
    sourcemap,
    lib: {
      entry: selected.entry,
      formats: ["es"],
      fileName: () => "index.js"
    },
    rollupOptions: {
      output: {
        inlineDynamicImports: true,
        entryFileNames: "index.js",
        chunkFileNames: "chunks/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]"
      }
    }
  }
});
