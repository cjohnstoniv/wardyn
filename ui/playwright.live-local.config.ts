/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import os from "node:os";
import path from "node:path";
import { defineConfig, devices } from "@playwright/test";

// The live-local harness's browser suites (docs/LIVE-TESTS.md): LL1 roles and
// LL4 AWS SSO through Entra, against the owner's real tenant. Its own config,
// so the hermetic and `live` projects in playwright.config.ts never see these
// files (they match *.live.ts, which neither project's testMatch does).
//
// Nothing is written inside the repository, and nothing that could hold a
// signed-in page is kept at all: no trace, no screenshot, no video, and a list
// reporter only.
export default defineConfig({
  testDir: "./e2e/live-local",
  testMatch: "*.live.ts",
  outputDir: path.join(os.tmpdir(), "wardyn-live-local"),
  reporter: [["list"]],
  retries: 0,
  workers: 1,
  timeout: 5 * 60_000,
  use: {
    ...devices["Desktop Chrome"],
    trace: "off",
    screenshot: "off",
    video: "off",
    actionTimeout: 60_000,
  },
});
