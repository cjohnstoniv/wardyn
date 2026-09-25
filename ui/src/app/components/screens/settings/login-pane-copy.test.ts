/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in door's canon (#628, the approved sign-in progress packet),
// pinned character for character: a reworded step or an ASCII hyphen where
// the packet has an em dash fails here, not on a screen.
import { describe, expect, it } from "vitest";
import { SIGNIN_PROGRESS } from "./login-pane-copy";

describe("SIGNIN_PROGRESS canon (#628)", () => {
  it("the three steps, as the packet draws them", () => {
    expect(SIGNIN_PROGRESS.STEP_START).toBe("Starting the sign-in sandbox");
    expect(SIGNIN_PROGRESS.STEP_DOWNLOAD).toBe("Downloading the sign-in image");
    expect(SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE).toBe("Downloading the sign-in image — first time only");
    expect(SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED).toBe("Downloading the sign-in image — failed");
    expect(SIGNIN_PROGRESS.STEP_WAIT("AWS")).toBe("Waiting for AWS");
    expect(SIGNIN_PROGRESS.STEP_WAIT("Claude")).toBe("Waiting for Claude");
  });

  it("the hints under the active step", () => {
    expect(SIGNIN_PROGRESS.DOWNLOAD_HINT).toBe("Can take a few minutes the first time.");
    expect(SIGNIN_PROGRESS.WAIT_HINT("AWS")).toBe("Waiting on AWS to hand back a verification link.");
  });

  it("the ready, opened and failed states' controls", () => {
    expect(SIGNIN_PROGRESS.OPEN("AWS")).toBe("Open AWS sign-in");
    expect(SIGNIN_PROGRESS.OPEN("Claude")).toBe("Open Claude sign-in");
    expect(SIGNIN_PROGRESS.COPY_LEAD).toBe("If nothing opens:");
    expect(SIGNIN_PROGRESS.COPY_LINK).toBe("copy the link");
    expect(SIGNIN_PROGRESS.TAB_OPEN("AWS")).toBe("The AWS sign-in tab is open. Waiting for your approval there.");
    expect(SIGNIN_PROGRESS.REOPEN).toBe("Reopen tab");
    expect(SIGNIN_PROGRESS.RETRY).toBe("Retry");
    expect(SIGNIN_PROGRESS.CANCEL).toBe("Cancel");
  });
});
