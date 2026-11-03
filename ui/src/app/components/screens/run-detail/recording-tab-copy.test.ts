/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../../../lib/types";
import { clockTime } from "../../../lib/format";
import { sessionOptionLabel } from "./recording-tab-copy";

const row = (data?: Record<string, unknown>): AuditEvent => ({
  id: "ev-1",
  time: "2026-11-03T10:00:00Z",
  actor_type: "system",
  actor: "wardynd",
  action: "session.recording.write",
  target: "run-1~agent",
  outcome: "success",
  data,
});

describe("sessionOptionLabel", () => {
  it("names a complete session plainly", () => {
    expect(sessionOptionLabel(row({ session: "agent" }))).toBe(`Session ended ${clockTime(row().time)} · wardynd`);
  });

  it("says incomplete when the session's row does, completeness unknown included", () => {
    for (const data of [{ incomplete: true, mask_gap: true }, { incomplete: true, completeness: "unknown" }]) {
      expect(sessionOptionLabel(row(data))).toBe(`Session ended ${clockTime(row().time)} · wardynd · incomplete`);
    }
  });
});
