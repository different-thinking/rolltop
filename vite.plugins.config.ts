// File overview: Vite library builds for runtime-loaded frontend plugin bundles.

import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const fromRoot = (path: string) => fileURLToPath(new URL(path, import.meta.url));

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
  plugins: [react()],
  define: {
    "process.env.NODE_ENV": JSON.stringify("production")
  },
  resolve: {
    alias: [
      // Matches the whole specifier, however deep the plugin sits — Vite
      // replaces only what the pattern covers, so an unanchored one would
      // splice the shim path into the middle of the import. Host modules a
      // plugin pulls in reach `components/Icon` by their own relative path and
      // are aliased by the same rule, which is the point: Phosphor's 4,543
      // modules must not enter a plugin bundle by any route.
      { find: /^.*\/components\/Icon$/, replacement: fromRoot("./frontend/src/plugins/shared/iconShim.ts") },
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
