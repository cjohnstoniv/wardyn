/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { defineConfig, devices } from "@playwright/test";

// Playwright E2E config — TWO projects:
//
//   chromium (hermetic, the default): specs in ui/e2e/ drive the built UI against
//   a seeded test backend (real wardynd + Postgres + the `none` runner, seeded
//   with deterministic fixtures and a fixed admin token) — NOT the full docker
//   stack, so PR runs are fast and hermetic. Run: `pnpm e2e` (or
//   --project=chromium).
//
// The hermetic backend URL is provided via WARDYN_E2E_BASE_URL (default
// localhost:8088). Start the seeded backend out-of-band (scripts/e2e-backend.sh)
// or set PLAYWRIGHT_WEB_SERVER to let Playwright manage it. Port 8088 (not 8080)
// avoids colliding with a developer's local compose stack on the default port.
const baseURL = process.env.WARDYN_E2E_BASE_URL || "http://localhost:8088";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: [
    ["list"],
    ["html", { outputFolder: "../test/reports/e2e/playwright-report", open: "never" }],
    ["junit", { outputFile: "../test/reports/e2e/junit.xml" }],
    ["json", { outputFile: "../test/reports/e2e/results.json" }],
  ],
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    { name: "chromium", testIgnore: ["screenshots/**"], use: { ...devices["Desktop Chrome"] } },
    {
      // screenshots: regenerates the docs/img UI PNGs (e2e/screenshots/docs.spec.ts)
      // against the dedicated backend booted by scripts/screenshots.sh. Its own
      // project so the hermetic chromium gate never runs it (chromium's testIgnore
      // drops screenshots/**). Fixed 1440×900 viewport for stable doc images.
      // Run: `make screenshots` (NOT in CI — no pixel gate).
      name: "screenshots",
      testMatch: "screenshots/**/*.spec.ts",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 900 },
      },
    },
  ],
});
