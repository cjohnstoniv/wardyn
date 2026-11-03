/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../../../lib/types";
import { clockTime } from "../../../lib/format";
import { RECORDING_MISSING_SESSION_BODY, sessionOptionLabel } from "./recording-tab-copy";

const time = new Date().toISOString();
const row = (data?: Record<string, unknown>): AuditEvent => ({
  id: "ev-1",
  time,
  actor_type: "system",
  actor: "wardynd",
  action: "session.recording.write",
  target: "run-1~agent",
  outcome: "success",
  data,
});

describe("sessionOptionLabel", () => {
  it("names a complete session plainly", () => {
    expect(sessionOptionLabel(row({ session: "agent" }))).toBe(`Session ended ${clockTime(time)} · wardynd`);
  });

  it("says incomplete when the session's row does, completeness unknown included", () => {
    for (const data of [{ incomplete: true, mask_gap: true }, { incomplete: true, completeness: "unknown" }]) {
      expect(sessionOptionLabel(row(data))).toBe(`Session ended ${clockTime(time)} · wardynd · incomplete`);
    }
  });
});

describe("recording tab copy (UXR-47)", () => {
  it("says terminal session and recording, never the file-format word cast", () => {
    expect(RECORDING_MISSING_SESSION_BODY).toBe("This terminal session's recording is missing. It may still be saving, or it failed to save.");
    expect(RECORDING_MISSING_SESSION_BODY).not.toMatch(/\bcast\b|attach session/i);
  });
});
