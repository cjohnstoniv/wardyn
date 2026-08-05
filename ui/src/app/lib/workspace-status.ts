/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace status → the THREE-word vocabulary the redesigned Workspaces list
// and detail pages render (mockup/wardyn-workspaces.js's wsStatusChip/
// storyFor). This module is the one place that folds the wire union down to
// the mock's three words. Pure TS — no React, no fetch, no DOM.

import type { Workspace, WorkspaceStatus } from "./types";
import { C } from "./workspace-copy";

export type StatusWord = "Setting up" | "Usable" | "Scan failed";

// pending_scan/scanning -> Setting up; scanned -> Usable; error -> Scan failed.
// Anything unrecognized defaults to Setting up, the safe "not ready yet" guess
// (the pre-collapse legacy statuses are gone from the wire — the server
// rewrote every stored row — so the union no longer carries them).
export function statusWord(status: WorkspaceStatus): StatusWord {
  switch (status) {
    case "pending_scan":
    case "scanning":
      return "Setting up";
    case "scanned":
      return "Usable";
    case "error":
      return "Scan failed";
    default:
      return "Setting up";
  }
}

export type StatusChipTone = "warning" | "info" | "success" | "danger";
export interface StatusToneInfo {
  tone: StatusChipTone;
  /** Only "scanning" pulses — the actively-in-progress nuance within Setting up. */
  pulse?: boolean;
}

// Chip nuance for statusWord's three buckets (mockup's wsStatusChip): Setting
// up splits into a plain warning (pending_scan) vs. a pulsing info dot
// (actively scanning); Usable is success; Scan failed is danger. Derived FROM
// statusWord so the two can never disagree about which bucket a status is in.
export function statusTone(status: WorkspaceStatus): StatusToneInfo {
  const word = statusWord(status);
  if (word === "Scan failed") return { tone: "danger" };
  if (word === "Usable") return { tone: "success" };
  return status === "scanning" ? { tone: "info", pulse: true } : { tone: "warning" };
}

// The one-line story under a workspace's name (mockup's storyFor), verbatim
// where the mock defines it — container images skip scanning entirely, so
// they get C.IMAGE_ENV appended exactly as the mock does.
export function storySentence(ws: Workspace): string {
  const word = statusWord(ws.status);
  if (word === "Scan failed") {
    return "The last scan failed, so there's no profile — a run gets no detected egress, secrets or services.";
  }
  if (word === "Setting up") {
    return ws.status === "scanning"
      ? "Scanning the source now."
      : "Not scanned yet — runs can attach it, nothing is attached automatically.";
  }
  return ws.kind === "container" ? `Runs can attach this now. ${C.IMAGE_ENV}` : "Runs can attach this now.";
}

// isUsable is the ONE predicate for "this workspace is done enough to attach".
// It exists because the terminal-success status changed from `ready` to
// `scanned` and four call sites went on comparing against the literal `ready`
// — so a workspace that was finished never earned its checkmark, never offered
// its re-scan action, and permanently wore a caution chip. Anything asking
// "is this workspace finished?" asks HERE, so the next rename moves one line.
export function isUsable(status: WorkspaceStatus): boolean {
  return statusWord(status) === "Usable";
}
