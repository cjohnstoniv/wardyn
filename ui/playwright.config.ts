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
//   stack, so PR runs are fast and hermetic.
//
// X2-F9: this file's own fullyParallel:true + multi-worker settings are safe
// ONLY under the isolation scripts/run-ui-e2e.sh provides (a fresh re-seeded
// backend per spec file, one spec at a time). They are NOT safe for a bare
// `pnpm e2e` / `pnpm exec playwright test` against ONE already-running
// backend — mutating specs (kill/approve/create/delete) would race each
// other on the same fixtures. The canonical, supported entry point is:
//   scripts/run-ui-e2e.sh              # all specs
//   scripts/run-ui-e2e.sh runs secrets # only runs.spec.ts + secrets.spec.ts
// `--project=chromium` selects this project when driving Playwright directly
// against a backend YOU know is exclusively yours.
//
// The hermetic backend URL is provided via WARDYN_E2E_BASE_URL (default
// localhost:8088). Start the seeded backend out-of-band (scripts/e2e-backend.sh)
// or set PLAYWRIGHT_WEB_SERVER to let Playwright manage it. Port 8088 (not 8080)
// avoids colliding with a developer's local compose stack on the default port.
const baseURL = process.env.WARDYN_E2E_BASE_URL || "http://localhost:8088";

// #469: scripts/run-ui-e2e.sh runs 2-3 lanes CONCURRENTLY (separate `playwright
// test` processes, each single-spec-at-a-time), so the html/junit reporters'
// FIXED output paths below would otherwise collide — two lanes writing the
// same playwright-report/ dir and junit.xml at once. WARDYN_E2E_REPORT_SUFFIX
// (set per-lane by run-ui-e2e.sh, e.g. "-lane0") keeps each lane's report
// files apart; empty by default, so every other caller (a bare `pnpm e2e`,
// the single-lane path) is byte-identical to before. The json reporter's own
// path here is unused by run-ui-e2e.sh — it always overrides via the
// PLAYWRIGHT_JSON_OUTPUT_NAME env var the json reporter itself honors — so it
// stays suffixed too, only for a caller driving Playwright directly.
const reportSuffix = process.env.WARDYN_E2E_REPORT_SUFFIX || "";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: [
    ["list"],
    ["html", { outputFolder: `../test/reports/e2e/playwright-report${reportSuffix}`, open: "never" }],
    ["junit", { outputFile: `../test/reports/e2e/junit${reportSuffix}.xml` }],
    ["json", { outputFile: `../test/reports/e2e/results${reportSuffix}.json` }],
  ],
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    { name: "chromium", testIgnore: ["screenshots/**", "demo/**", "live/**"], use: { ...devices["Desktop Chrome"] } },
    {
      // live: the LIVE walks (e2e/live/*.spec.ts) — specs that drive a REAL,
      // already-running Wardyn rather than the hermetic `-runner none` backend.
      // Today that is the kind SSO cluster (scripts/kind-sso-walk.sh): two Dex
      // principals, the k8s runner substrate, real sandboxes, and AWS SSO
      // pointed at test/awsssofake. It is its OWN project so the hermetic
      // chromium gate never runs it (chromium's testIgnore drops live/**), and
      // every spec in it self-skips without WARDYN_TEST_K8S=1 — the same guard
      // the other cluster-dependent lanes use, and the reason a bare `pnpm e2e`
      // can never point a browser at somebody's live cluster and start clicking.
      //
      // It inherits the top-level baseURL, which run-ui-e2e.sh's LIVE mode sets
      // from WARDYN_E2E_LIVE_BASE_URL — one spelling, not two.
      //
      // No retries and a long timeout, for the reasons the demo project gives:
      // the walk launches real sandboxes on a cluster (image pulls, pod
      // scheduling, a device-code login in a PTY), and a retry would replay a
      // sign-in whose captured credential the first attempt already consumed.
      name: "live",
      testMatch: "live/**/*.spec.ts",
      retries: 0,
      timeout: 30 * 60_000,
      // A single ACTION may never wait out the 30-minute test budget: a missing
      // element (0.7.5's first walk: the admin was on the welcome gate, not on
      // Settings) must cost three minutes, not the whole walk. Long waits in
      // these specs are expect.poll calls with their own explicit timeouts.
      use: { ...devices["Desktop Chrome"], actionTimeout: 180_000 },
    },
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
      // share its base URL. HEADLESS, deliberately: the browser records
      // ITSELF (recordVideo), so nothing needs to be on screen — and headed
      // recording could never be pixel-clean on this box: the screencast
      // captures the window's VISIBLE content area, WSLg clamps any window
      // taller than the screen, so the page could never reach a true 1080
      // and every take shipped a ~90px gray letterbox at the bottom.
      // Headless frames are exactly the viewport. The cost is honest: there
      // is no live window to watch a take in progress — the published cut is
      // the first viewing. Driven by scripts/record-demo.sh; the spec
      // self-skips without WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a
      // browser at a developer's live stack and start clicking Launch.
      //
      // Fixed 1920x1080 viewport, matching video.size EXACTLY. viewport:null
      // ("let the window decide") shipped a gray bar on the bottom of every
      // take: the page's content area is the window MINUS Chrome's own UI
      // (~90px of tab strip + toolbar), so the real page was ~1920x990 and
      // Playwright letterboxed it into the 1080-tall recording with gray.
      // A forced viewport is recorded pixel-for-pixel regardless of what the
      // window chrome eats; the only cost is that the LIVE on-screen window
      // clips the bottom ~90px (the recording does not — it captures the
      // page, not the window).
      // No retries: a retry would restart the recording halfway through.
      name: "demo",
      testMatch: "demo/**/*.spec.ts",
      retries: 0,
      timeout: 30 * 60_000,
      use: {
        // Deliberately NOT ...devices["Desktop Chrome"]: the preset's fields
        // (a fake UA, its own viewport, touch flags) are all things this
        // project sets — or wants unset — itself.
        baseURL: process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080",
        headless: true,
        viewport: { width: 1920, height: 1080 },
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
        // No window args: headless has no window to position, and the old
        // --window-size request was moot anyway (WSLg clamped it).
        launchOptions: {},
      },
    },
  ],
});
