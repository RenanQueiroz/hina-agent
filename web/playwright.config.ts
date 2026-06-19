import { defineConfig, devices } from "@playwright/test";

// Drives the built UI served by the Go binary. Start the server separately and
// pass E2E_BASE_URL + E2E_BOOTSTRAP_PW (see the smoke script / CI).
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://127.0.0.1:8733",
    headless: true,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
