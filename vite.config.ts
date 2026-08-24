// File overview: Vite build configuration for the React frontend bundle that the Go server serves.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Sourcemaps are the single largest thing these builds write — several times
// the bundle they describe — and the image build pays for every byte twice,
// once generating it and again when the layer is snapshotted and pushed.
// Nothing serves them, so the `Dockerfile` sets this to `0`. Every other
// build keeps them: this defaults to on precisely so a local or CI build is
// still debuggable without anyone remembering a flag.
const sourcemap = process.env.ROLLTOP_BUILD_SOURCEMAPS !== "0";

export default defineConfig({
  root: "frontend",
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/attachments": "http://127.0.0.1:8080",
      "/blobs": "http://127.0.0.1:8080",
      "/plugins": "http://127.0.0.1:8080"
    }
  }
});
