/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../../../lib/types";
import { clockTime } from "../../../lib/format";
import { RECORDING_MISSING_SESSION_BODY, defaultSessionKey, sessionNotes, sessionOptionLabel } from "./recording-tab-copy";

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
  it("names a person's session by its end and actor", () => {
    expect(sessionOptionLabel(row({ session: "console-1" }))).toBe(`Session ended ${clockTime(time)} · wardynd`);
    expect(sessionOptionLabel(row(undefined))).toBe(`Session ended ${clockTime(time)} · wardynd`);
  });

  it("names the agent's output cast, and the one re-read after a restart, in words", () => {
    expect(sessionOptionLabel(row({ session: "agent" }))).toBe(`Agent output · ended ${clockTime(time)}`);
    expect(sessionOptionLabel(row({ session: "agent-recovered", recovered: true }))).toBe(`Agent output, re-read after a restart · ended ${clockTime(time)}`);
  });

  it("keeps the incomplete suffix on every kind of label", () => {
    expect(sessionOptionLabel(row({ session: "agent", incomplete: true, read_error: true }))).toBe(`Agent output · ended ${clockTime(time)} · incomplete`);
    expect(sessionOptionLabel(row({ session: "agent-recovered", incomplete: true }))).toBe(`Agent output, re-read after a restart · ended ${clockTime(time)} · incomplete`);
  });

  it("says incomplete when the session's row does, completeness unknown included", () => {
    for (const data of [{ incomplete: true, mask_gap: true }, { incomplete: true, completeness: "unknown" }]) {
      expect(sessionOptionLabel(row(data))).toBe(`Session ended ${clockTime(time)} · wardynd · incomplete`);
    }
  });
});

describe("sessionNotes", () => {
  it("says nothing for a whole recording or no row", () => {
    expect(sessionNotes(row({ session: "agent" }))).toEqual([]);
    expect(sessionNotes(undefined)).toEqual([]);
  });

  it.each([
    [{ incomplete: true, read_error: true }, ["The runner's copy ended early; the rest of the output is missing."]],
    [{ incomplete: true, mask_gap: true }, ["Some output was left out because Wardyn couldn't check it for secrets."]],
    [{ truncated: true, limit_bytes: 8 << 20 }, ["Only the first 8 MiB was kept."]],
    [{ gap_bytes: 9437184 }, ["The start of the output is missing; the runner had already discarded it."]],
    [{ incomplete: true, completeness: "unknown" }, ["Wardyn couldn't confirm this copy is complete."]],
  ])("words %j on its own line", (data, want) => {
    expect(sessionNotes(row({ session: "agent", ...data }))).toEqual(want);
  });

  it("gives one line per flag", () => {
    expect(sessionNotes(row({ session: "agent", incomplete: true, read_error: true, truncated: true, limit_bytes: 8 << 20, gap_bytes: 5 }))).toHaveLength(3);
  });
});

describe("defaultSessionKey", () => {
  const at = (id: string, session: string, mins: number): AuditEvent => ({ ...row({ session }), id, target: `run-1~${session}-${id}`, time: new Date(Date.parse(time) + mins * 60_000).toISOString() });
  const rows = [at("a", "agent", 0), at("b", "console-1", 5), at("c", "agent-recovered", 3)];

  it("opens a macOS run on its newest agent cast, not the newest session", () => {
    expect(defaultSessionKey("run-1", true, rows, ["run-1~desktop-x"])).toBe("run-1~agent-recovered-c");
  });
  it("falls back to the newest session, then a screen recording, then the run's own key", () => {
    expect(defaultSessionKey("run-1", true, [rows[1], at("d", "console-2", 9)], [])).toBe("run-1~console-2-d");
    expect(defaultSessionKey("run-1", true, [], ["run-1~desktop-x"])).toBe("run-1~desktop-x");
    expect(defaultSessionKey("run-1", true, [], [])).toBe("run-1");
  });
  it("leaves every other run on its own key", () => {
    expect(defaultSessionKey("run-1", false, rows, ["run-1~desktop-x"])).toBe("run-1");
  });
});

describe("recording tab copy (UXR-47)", () => {
  it("says terminal session and recording, never the file-format word cast", () => {
    expect(RECORDING_MISSING_SESSION_BODY).toBe("This terminal session's recording is missing. It may still be saving, or it failed to save.");
    expect(RECORDING_MISSING_SESSION_BODY).not.toMatch(/\bcast\b|attach session/i);
  });
});
