/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace status → the words the console renders. This module is the one
// place that folds the wire union down to them. Pure TS — no React, no fetch,
// no DOM.
//
// The vocabulary shrank in 0.5, because the thing it described went away. It
// used to be "Setting up" / "Usable" / "Scan failed", modelling a pipeline: a
// workspace was born pending_scan, a scan run promoted it to scanned, and a
// failed scan left it in error.
//
// The 0.5 dialog does one POST and no scan/build/verify, so nothing promotes a
// workspace any more — every writer of `scanned` targets a SOURCE row, and the
// workspace's own writer only ever writes `error`, from a branch nothing can
// reach. A new workspace therefore sat at pending_scan forever and the console
// showed a permanent amber "Setting up": a progress indicator for work that was
// never going to happen, on a workspace that runs could attach the whole time
// (resolveCreateRunImage is fail-open by design).
//
// So "Setting up" is gone. A workspace is Ready when it exists. `error` is kept
// because a row can still carry it from before the upgrade, and it names what
// actually failed — an import — rather than a scan that no longer exists.

import type { Workspace, WorkspaceStatus } from "./types";

export type StatusWord = "Ready" | "Import failed";

// `scanned` is the modern usable state. `pending_scan`/`scanning` are legacy
// rows from before 0.5 (migration 0036 heals them) and are equally usable —
// nothing was ever going to scan them, and nothing needed to. Anything
// unrecognized reads Ready for the same reason: attachment never depended on
// this field.
export function statusWord(status: WorkspaceStatus): StatusWord {
  return status === "error" ? "Import failed" : "Ready";
}

export type StatusChipTone = "warning" | "info" | "success" | "danger";
export interface StatusToneInfo {
  tone: StatusChipTone;
  /** Retained for callers that render a live pulse; nothing pulses now that
   *  there is no in-progress state to pulse for. */
  pulse?: boolean;
}

export function statusTone(status: WorkspaceStatus): StatusToneInfo {
  return statusWord(status) === "Import failed" ? { tone: "danger" } : { tone: "success" };
}

// The one-line story under a workspace's name. It answers "can I use this, and
// what is it?" — not "how far along is a pipeline", which is the question the
// old copy answered and 0.5 stopped having.
export function storySentence(ws: Workspace): string {
  if (statusWord(ws.status) === "Import failed") {
    return "An import run failed against this workspace. Runs can still attach it — nothing was detected for them automatically.";
  }
  return "Runs can attach this now.";
}

// isUsable is the ONE predicate for "this workspace is done enough to attach".
// It exists because the terminal-success status changed from `ready` to
// `scanned` and four call sites went on comparing against the literal `ready`
// — so a finished workspace never earned its checkmark. Anything asking "is
// this workspace finished?" asks HERE, so the next rename moves one line.
//
// It stays true even for `error`: an import failure never blocked attachment,
// and pretending otherwise would hide a usable workspace behind a red chip.
export function isUsable(_status: WorkspaceStatus): boolean {
  return true;
}
