/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordingTab's session-picker copy (run-detail.tsx), split out under that
// file's line cap.
//
// session.recording.write is emitted at DETACH (internal/api/attach.go's finish),
// not at attach — the picker must not name the attach end of the session.
import { clockTime, fmtBytes } from "../../../lib/format";
import type { AuditEvent } from "../../../lib/types";
import { RECORDING_COMPLETENESS } from "../../wardyn/copy/recording-level";

// The agent's output cast of a macOS VM task run is saved under session "agent", or "agent-recovered" when
// a restart lost it and the server re-read it (internal/api/run_agent_cast.go); both are Wardyn's, not a person's.
const isAgentCast = (e: AuditEvent): boolean => e.data?.session === "agent" || e.data?.session === "agent-recovered";

export function sessionOptionLabel(e: AuditEvent): string {
  const incomplete = e.data?.incomplete === true ? " · incomplete" : "";
  const ended = clockTime(e.time);
  if (e.data?.session === "agent") return `Agent output · ended ${ended}${incomplete}`;
  if (e.data?.session === "agent-recovered") return `Agent output, re-read after a restart · ended ${ended}${incomplete}`;
  return `Session ended ${ended} · ${e.actor}${incomplete}`;
}

// The cast's cap, as its row says it: whole MiB read as "8 MiB".
function limitText(bytes: unknown): string {
  if (typeof bytes !== "number" || bytes <= 0) return "part";
  return bytes % (1 << 20) === 0 ? `${bytes / (1 << 20)} MiB` : fmtBytes(bytes);
}

/** One line for each way the session's row says its recording is not whole; none for a whole one. */
export function sessionNotes(e: AuditEvent | undefined): string[] {
  const d = e?.data;
  if (!d) return [];
  const notes: string[] = [];
  if (d.incomplete === true && d.read_error === true) notes.push(RECORDING_COMPLETENESS.READ_ERROR);
  if (d.mask_gap === true) notes.push(RECORDING_COMPLETENESS.MASK_GAP);
  if (d.truncated === true) notes.push(RECORDING_COMPLETENESS.TRUNCATED(limitText(d.limit_bytes)));
  if (typeof d.gap_bytes === "number" && d.gap_bytes > 0) notes.push(RECORDING_COMPLETENESS.GAP_BYTES);
  // An incomplete row that names no cause (a swept cast whose header could not be read) is one Wardyn cannot vouch for.
  if (d.incomplete === true && d.read_error !== true && d.mask_gap !== true) notes.push(RECORDING_COMPLETENESS.UNKNOWN);
  return notes;
}

/**
 * The cast the Recording tab opens on. A macOS VM run never has a cast under its own key (its sandbox is not
 * asked to record), so it opens on the newest agent output cast, else the newest session of any kind; every
 * other run opens on its own. `desktopKeys` are the run's screen recordings, the last resort.
 */
export function defaultSessionKey(runId: string, macOS: boolean, sessions: AuditEvent[], desktopKeys: string[]): string {
  if (!macOS) return runId;
  const newest = (rows: AuditEvent[]) => rows.reduce<AuditEvent | undefined>((a, e) => (!a || Date.parse(e.time) >= Date.parse(a.time) ? e : a), undefined);
  return newest(sessions.filter(isAgentCast))?.target ?? newest(sessions)?.target ?? desktopKeys[0] ?? runId;
}

// F1-F11: picking an attach session whose cast is
// missing must render this SESSION-scoped copy, not the RUN-scoped "This run
// has no captured terminal session" — a fact about the whole run must never
// be asserted from a fetch that only ever checked one SPECIFIC session.
// selected !== runId (run-detail.tsx) is what distinguishes the two cases.
export const RECORDING_MISSING_SESSION_TITLE = "No recording for this session";
export const RECORDING_MISSING_SESSION_BODY =
  "This terminal session's recording is missing. It may still be saving, or it failed to save.";
