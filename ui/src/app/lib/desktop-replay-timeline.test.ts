/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { DesktopPlayback } from "./api/recordings";
import type { AuditEvent } from "./types";
import { advance, frameMs, gapAt, jumpTo, pct, replayActions, replayDuration, replayGaps } from "./desktop-replay-timeline";
import { RECORDING_REPLAY as C } from "../components/wardyn/copy/recording-level";

const START = "2026-10-20T10:40:00Z";
const at = (s: number) => new Date(Date.parse(START) + s * 1000).toISOString();

function pb(over: Partial<DesktopPlayback> = {}): DesktopPlayback {
  return { version: 1, format: "novnc-playback", encoding: "base64", started_at: START, frames: ["{0{AAAA", "{600000{AAAA"], ...over };
}

function row(id: string, action: string, s: number, over: Partial<AuditEvent> = {}): AuditEvent {
  return { id, time: at(s), actor_type: "system", actor: "wardynd", action, outcome: "success", ...over };
}

const pause = row("p1", "desktop.recording.pause", 138, { data: { reason: "you took over to sign in", gap_start: at(138) } });
const resume = row("r1", "desktop.recording.resume", 272, {
  data: { reason: "you took over to sign in", gap_start: at(138), gap_end: at(272), gap_ms: 134_000 },
});

describe("desktop replay timeline", () => {
  it("reads a frame's time and runs to the latest of frames, gaps and the write row", () => {
    expect(frameMs("{600000{AAAA")).toBe(600_000);
    expect(replayDuration(pb())).toBe(600_000);
    expect(replayDuration(pb({ gaps: [{ start_ms: 1, end_ms: 700_000, reason: "x" }] }))).toBe(700_000);
    expect(replayDuration(pb(), at(800))).toBe(800_000);
  });

  it("marks a gap from its pause and resume rows, naming both", () => {
    const gaps = replayGaps(pb(), 600_000, [pause, resume]);
    expect(gaps).toEqual([{ startMs: 138_000, endMs: 272_000, reason: "you took over to sign in", pauseId: "p1", resumeId: "r1" }]);
  });

  it("runs a pause with no resume row to the recorder's own gap end, else to the end", () => {
    const doc = pb({ gaps: [{ start_ms: 138_100, end_ms: 200_000, reason: "you took over to sign in" }] });
    expect(replayGaps(doc, 600_000, [pause])).toEqual([
      { startMs: 138_000, endMs: 200_000, reason: "you took over to sign in", pauseId: "p1", resumeId: undefined },
    ]);
    expect(replayGaps(pb(), 600_000, [pause])[0].endMs).toBe(600_000);
  });

  it("keeps a recorder gap no row covers, and does not double one a row covers", () => {
    const doc = pb({ gaps: [{ start_ms: 0, end_ms: 5_000, reason: "you took over" }, { start_ms: 138_050, end_ms: 271_900, reason: "x" }] });
    const gaps = replayGaps(doc, 600_000, [pause, resume]);
    expect(gaps.map((g) => [g.startMs, g.endMs])).toEqual([
      [0, 5_000],
      [138_000, 272_000],
    ]);
  });

  it("finds the gap the playhead is in, start inclusive and end exclusive", () => {
    const gaps = replayGaps(pb(), 600_000, [pause, resume]);
    expect(gapAt(gaps, 137_999)).toBeUndefined();
    expect(gapAt(gaps, 138_000)?.pauseId).toBe("p1");
    expect(gapAt(gaps, 272_000)).toBeUndefined();
  });

  it("lists the desktop's actions inside the recording in the boards' words, blocked ones flagged", () => {
    const audit = [
      row("c1", "ui.clipboard.transfer", 240, { actor_type: "human", target: "paste_in", data: { direction: "paste_in", bytes: 214, sha256: "9f2c00000000a41e" } }),
      row("c2", "ui.clipboard.transfer", 360, { actor_type: "human", outcome: "denied", target: "copy_out", data: { direction: "copy_out", bytes: 86 } }),
      row("f1", "desktop.file.transfer", 420, { actor_type: "human", data: { direction: "upload", bytes: 49_152, sha256: "c3a100000000e9d0" } }),
      row("x1", "run.complete", 400),
      row("early", "ui.clipboard.transfer", -10, { data: { direction: "paste_in" } }),
      pause,
      resume,
    ];
    const acts = replayActions(pb(), 600_000, audit);
    expect(acts.map((a) => [a.id, a.actor, a.text, a.blocked])).toEqual([
      ["p1", "SYSTEM", C.ACT_PAUSED, false],
      ["c1", "HUMAN", C.ACT_PASTE, false],
      ["r1", "SYSTEM", C.ACT_RESUMED, false],
      ["c2", "HUMAN", C.ACT_COPY_TRIED, true],
      ["f1", "HUMAN", C.ACT_UPLOAD, false],
    ]);
    expect(acts[1].detail).toBe("214 B · sha256 9f2c…a41e");
    expect(acts[0].detail).toBe("reason: you took over to sign in");
    expect(acts[2].detail).toBe("gap 2 min 14 s");
  });

  it("jumps to the clicked action's moment, clamped to the recording", () => {
    const [a] = replayActions(pb(), 600_000, [row("c1", "ui.clipboard.transfer", 240, { data: { direction: "paste_in" } })]);
    expect(jumpTo(a, 600_000)).toBe(240_000);
    expect(jumpTo({ ...a, ms: 900_000 }, 600_000)).toBe(600_000);
  });

  it("advances the playhead at speed and stops at the end; places a position as a share", () => {
    expect(advance(1_000, 250, 4, 600_000)).toBe(2_000);
    expect(advance(599_900, 250, 1, 600_000)).toBe(600_000);
    expect(pct(150_000, 600_000)).toBe("25%");
  });
});
