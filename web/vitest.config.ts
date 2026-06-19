import { defineConfig } from "vitest/config";

// Vitest covers unit tests under src/. The Playwright e2e specs (e2e/) are
// excluded so `vitest run` doesn't try to load Playwright's test() and fail.
export default defineConfig({
  test: {
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
    exclude: ["e2e/**", "node_modules/**", "dist/**"],
    environment: "node",
  },
});
