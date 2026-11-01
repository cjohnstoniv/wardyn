/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import os from "node:os";
import path from "node:path";
import { defineConfig, devices } from "@playwright/test";

// The macOS VM check's console rows (e2e/macos-vm/, scripts/macos-vm-check.sh --console): specs that drive
// the console of a REAL, already-running Wardyn with a real macOS VM run, from the Mac under test. Its own
// config, so the hermetic project in playwright.config.ts never sees these files (that project's testIgnore
// drops macos-vm/**), and no backend is booted or faked here.
//
// Inputs come from the environment and every spec skips, saying so, when one is unset:
//   WARDYN_MACVM_CONSOLE_URL         the console's address
//   WARDYN_MACVM_CONSOLE_RUN_ID      the macOS VM run to look at
//   WARDYN_MACVM_CONSOLE_TOKEN_FILE  a file holding an admin token; the path travels, the token never does
//   WARDYN_MACVM_CONSOLE_OUT         where the JSON per check is written (default: a temp directory)
// and, optionally, a marker each of the Files and Output specs looks for:
//   WARDYN_MACVM_CONSOLE_FILE_NAME, WARDYN_MACVM_CONSOLE_OUTPUT_MARK
//
// Nothing a signed-in page could hold is kept: no trace, no screenshot, no video, a list reporter only, and
// the output directory sits outside the repository.
export default defineConfig({
  testDir: "./e2e/macos-vm",
  testMatch: "*.spec.ts",
  outputDir: path.join(os.tmpdir(), "wardyn-macvm-console-pw"),
  reporter: [["list"]],
  fullyParallel: false,
  retries: 0,
  workers: 1,
  timeout: 3 * 60_000,
  use: {
    ...devices["Desktop Chrome"],
    baseURL: process.env.WARDYN_MACVM_CONSOLE_URL || undefined,
    trace: "off",
    screenshot: "off",
    video: "off",
    actionTimeout: 60_000,
  },
});
