/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { RUN_STARTUP } from "./run-status-detail";
import { RUN_COCKPIT } from "../wardyn/copy";

// #1419: the owner-approved run-startup-progress packet's strings are the app's
// strings, character for character (the em dashes and the full stops included).
// Editing one is a copy change and needs the mock's owner sign-off.
describe("RUN_STARTUP canon", () => {
  it("is exactly the approved packet's strings", () => {
    expect(RUN_STARTUP).toStrictEqual({
      STEP_BUILD: "Building the image",
      STEP_START: "Starting the sandbox",
      STEP_TERMINAL: "Opening the terminal",
      STEP_TASK: "Starting the task",
      STEP_COMMAND: "Starting the command",
      STEP_DOWNLOAD_FAILED: "Downloading the image — failed",
      STEP_START_FAILED: "Starting the sandbox — failed",
      BUILD_HINT: "This run needs its own image, so it is built first. This can take several minutes.",
      SLOW: "Still starting. A first start may need to download the image, which can take a few minutes.",
      OVERDUE:
        "This is taking longer than a start usually does. If nothing changes, kill the run and launch it again.",
    });
  });

  it("retires RUN_COCKPIT.starting (the last row says it now)", () => {
    expect("starting" in RUN_COCKPIT).toBe(false);
  });
});
