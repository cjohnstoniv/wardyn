/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display helpers for the user-drives surfaces. They decide how a FROZEN
// string is PRESENTED, never what it says — the same discipline
// governance/display.tsx states for its own withMono. Pure TS, no React, so
// the admin table, the member's New Run card and the Getting Started chip can
// all reach it without pulling a component graph.
import { DRIVES } from "./user-drives-copy";

// THE one size helper (user-drives-prompt.md §5 #10): SIZE_MIB, or SIZE_GIB
// for a whole multiple of 1024 — never lib/format.ts's fmtBytes, which labels
// a binary quotient "MB" and stops at megabytes.
//
// Returns null for 0/absent rather than a string, because the two audiences
// spell that state differently and neither may be chosen here: a member's
// sentence takes the _NOSIZE twin (NR_HINT_NOSIZE / GS_DRIVE_CHIP_NOSIZE),
// while an admin's size cell renders DRIVES.SIZE_NONE. size_mib is omitempty
// on the wire, so `undefined` and `0` are the same state.
export function driveSizeLabel(sizeMib?: number): string | null {
  if (!sizeMib || sizeMib <= 0) return null;
  return sizeMib % 1024 === 0 ? DRIVES.SIZE_GIB(sizeMib / 1024) : DRIVES.SIZE_MIB(sizeMib);
}

// The INLINE mode words — the {mode} placeholder in NR_HINT and
// GS_DRIVE_CHIP, which is the mode of the MOUNT being described, not of the
// allocation: a writable allocation narrowed by NR_READONLY_TOGGLE for this
// run passes writable=false, so the sentence beside it never promises
// persistence a read-only mount cannot give.
export function driveModeWord(writable: boolean): string {
  return writable ? DRIVES.MODE_RW_INLINE : DRIVES.MODE_RO_INLINE;
}
