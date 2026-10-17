import { defineConfig, devices } from "@playwright/test";

// Runs against a running stack: `docker compose up` (console on :8500) or `FULL=1 scripts/seed_host.sh`
// (E2E_BASE_URL=http://127.0.0.1:18500). The stack must hold seeded open cases (`make seed`).
// screenshots.spec.ts rewrites docs/images/ and only runs under `make docs` (E2E_SCREENSHOTS=1).
export default defineConfig({
  testDir: ".",
  testIgnore: process.env.E2E_SCREENSHOTS ? [] : ["screenshots.spec.ts"],
  outputDir: "../../artifacts/e2e/results",
  reporter: [
    ["list"],
    ["html", { outputFolder: "../../artifacts/e2e/report", open: "never" }],
  ],
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 5_000 },
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://127.0.0.1:8500",
    trace: "retain-on-failure",
    colorScheme: "dark",
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1280, height: 800 },
      },
    },
  ],
});
