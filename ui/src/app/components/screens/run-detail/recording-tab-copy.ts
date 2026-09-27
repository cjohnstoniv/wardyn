/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RecordingTab's session-picker copy (run-detail.tsx), split out under that
// file's line cap.
//
// session.recording.write is emitted at DETACH (internal/api/attach.go's finish),
// not at attach — the picker must not name the attach end of the session.
import { clockTime } from "../../../lib/format";
import type { AuditEvent } from "../../../lib/types";

// DRAFT (M2 canon pending)
export function sessionOptionLabel(e: AuditEvent): string {
  return `Session ended ${clockTime(e.time)} · ${e.actor}`;
}

// DRAFT (M2 canon pending) — F1-F11: picking an attach session whose cast is
// missing must render this SESSION-scoped copy, not the RUN-scoped "This run
// has no captured terminal session" — a fact about the whole run must never
// be asserted from a fetch that only ever checked one SPECIFIC session.
// selected !== runId (run-detail.tsx) is what distinguishes the two cases.
export const RECORDING_MISSING_SESSION_TITLE = "No recording for this session";
export const RECORDING_MISSING_SESSION_BODY =
  "This attach session's cast is missing — it may still be capturing, or the recording failed to save.";
