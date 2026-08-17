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
    { name: "chromium", testIgnore: ["screenshots/**", "demo/**"], use: { ...devices["Desktop Chrome"] } },
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
    {
      // demo: drives the screen recording (e2e/demo/walkthrough.spec.ts) against
      // the REAL compose stack on :8080 — a live runner, real sandboxes, a
      // connected model. The hermetic `-runner none` backend the chromium gate
      // uses cannot start a demo sandbox, so this project deliberately does not
      // share its base URL. Headed, because something has to be on screen to
      // film. Driven by scripts/record-demo.sh (`make record-demo`); the spec
      // self-skips without WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a
      // browser at a developer's live stack and start clicking Launch.
      //
      // viewport:null hands sizing to the real window (--window-size below), so
      // the captured frame is the browser, not a letterboxed page inside it.
      // No retries: a retry would restart the recording halfway through.
      name: "demo",
      testMatch: "demo/**/*.spec.ts",
      retries: 0,
      timeout: 30 * 60_000,
      use: {
        // Deliberately NOT ...devices["Desktop Chrome"]: that preset carries
        // deviceScaleFactor, which Playwright refuses to combine with a null
        // viewport ("deviceScaleFactor is not supported with null viewport").
        // The preset's other fields (a fake UA, a fixed viewport, touch flags)
        // are all things this project wants the real window to decide anyway.
        baseURL: process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080",
        headless: false,
        viewport: null,
        // The browser records ITSELF. A desktop grab of this window is at the
        // mercy of whatever else is on that monitor: WSLg presents these as
        // RAIL windows, so from Linux we can neither raise them reliably nor
        // even read the true z-order (X stacking is not the Windows
        // compositor's). A take once filmed a browser game that was sitting on
        // top of the frame for six minutes. Playwright's capture comes from
        // inside the page, so occlusion and window position cannot corrupt it.
        // The OS cursor is absent either way — overlay.ts draws its own.
        video: { mode: "on", size: { width: 1920, height: 1080 } },
        // Playwright's default action timeout is UNLIMITED, so a locator that
        // matches nothing parks the driver on screen until the whole act's
        // budget expires — it looks exactly like the app hanging, or like the
        // driver waiting on a human (it never does). Fail the click instead.
        // Generous, because these clicks land on a real, busy console.
        actionTimeout: 45_000,
        trace: "off",
        launchOptions: {
          args: ["--window-position=0,0", "--window-size=1920,1080", "--hide-crash-restore-bubble"],
        },
      },
    },
  ],
});
