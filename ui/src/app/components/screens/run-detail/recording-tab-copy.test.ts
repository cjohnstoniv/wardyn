/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { RECORDING_MISSING_SESSION_BODY } from "./recording-tab-copy";

describe("recording tab copy (UXR-47)", () => {
  it("says terminal session and recording, never the file-format word cast", () => {
    expect(RECORDING_MISSING_SESSION_BODY).toBe("This terminal session's recording is missing. It may still be saving, or it failed to save.");
    expect(RECORDING_MISSING_SESSION_BODY).not.toMatch(/\bcast\b|attach session/i);
  });
});
