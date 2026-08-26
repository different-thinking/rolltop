// File overview: Vitest configuration for the frontend unit tests. Kept
// separate from vite.config.ts so the build config stays about building; this
// only concerns the test runner. Tests live next to the code they cover as
// *.test.ts(x) under frontend/src.

import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // happy-dom gives the pure functions that reach for window/document (timers,
    // DOM helpers) a lightweight browser environment without a real browser.
    environment: "happy-dom",
    include: ["frontend/src/**/*.test.{ts,tsx}"],
  },
});
