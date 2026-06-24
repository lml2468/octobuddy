/// <reference types="vitest" />
import { defineConfig } from "vitest/config";

// Vitest config kept separate from vite.config.ts so the production build
// pipeline (which pulls @wailsio/runtime's vite plugin and assumes the
// real Wails runtime) doesn't pull in test-only deps. jsdom env lets the
// Wails binding files import without exploding on `window` access at
// module top-level — the actual binding methods are mocked per test.
export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["tests/**/*.test.ts"],
  },
});
