/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace status → the THREE-word vocabulary the redesigned Workspaces list
// and detail pages render (mockup/wardyn-workspaces.js's wsStatusChip/
// storyFor). The mock's own state model is simpler (setting_up/usable/
// scan_failed, plus a separate `scanning` flag) than the real wire
// WorkspaceStatus union, which still carries the pre-verify-removal statuses
// (building/build_error/verifying/verify_failed/ready) on older rows until the
// migration wave (see import-types.ts's activeStepForStatus) — this module is
// the one place that folds the real union down to the mock's three words.
// Pure TS — no React, no fetch, no DOM.

import type { Workspace, WorkspaceStatus } from "./types";
import { C } from "./workspace-copy";

export type StatusWord = "Setting up" | "Usable" | "Scan failed";

// pending_scan/scanning -> Setting up; scanned -> Usable; error -> Scan failed.
// LEGACY-tolerant: ready/verifying/verify_failed/building/build_error were all
// reached only by way of a scan that already SUCCEEDED, and Verify/Finalize no
// longer gate usability (the import panel's Verify/Finalize steps are
// retired) — so they read as Usable now, same as a plain `scanned` row.
// Anything unrecognized defaults to Setting up, the safe "not ready yet" guess.
export function statusWord(status: WorkspaceStatus): StatusWord {
  switch (status) {
    case "pending_scan":
    case "scanning":
      return "Setting up";
    case "scanned":
    case "ready":
    case "verifying":
    case "verify_failed":
    case "building":
    case "build_error":
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
