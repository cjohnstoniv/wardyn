/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display helpers shared by the /drives surfaces. None of them invents copy:
// they decide how a FROZEN string is presented, never what it says.
//
// Note / noteClass / question are IMPORTED from the governance screen's own
// display.tsx rather than copied a third time — governance/display.tsx's header
// names this as the moment to stop duplicating them, and one import beats a
// third set of ten lines. They are pure presentation with no governance copy in
// them.
import * as React from "react";
import { DRIVES } from "../../../lib/user-drives-copy";
import { driveSizeLabel } from "../../../lib/user-drives-display";
import type { StorageEnforcement } from "../../../lib/api/drives";
import { Mono } from "../../wardyn/code-block";

export { Note, noteClass, question } from "../governance/display";

// Backtick-mono rendering (user-drives-prompt.md §7 header rule): a backticked
// substring inside a frozen string is the mount target, an env var or a wire
// value, and renders font-mono — uniformly, at EVERY recurrence, not only where
// the container happened to accept a node.
//
// The copy module carries these as plain text on purpose (its own header rule),
// so this is the ONE place that decides which substrings get the span. A DRIVE
// NAME is never here: §7 renders it inside double quotes because it is a
// human-chosen label, not a literal.
const MONO_TERMS = ["WARDYN_USER_DRIVE_HOST_ROOTS", "/home/agent/drive", "disk_mib", ". _ -"];
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
const MONO_RE = new RegExp(`(${MONO_TERMS.map(escape).join("|")})`, "g");

export function withMono(text: string): React.ReactNode {
  return text.split(MONO_RE).map((part, i) =>
    MONO_TERMS.includes(part) ? (
      <Mono key={i} className="text-inherit">
        {part}
      </Mono>
    ) : (
      <React.Fragment key={i}>{part}</React.Fragment>
    ),
  );
}

// The admin's spelling of a size: THE one size helper (lib/user-drives-display
// .ts's driveSizeLabel, §5 #10 — never lib/format.ts's fmtBytes), with the
// admin half of the state it deliberately refuses to choose. driveSizeLabel
// returns null for 0/absent because a member's sentence takes the _NOSIZE twin
// there while an admin's cell renders SIZE_NONE; this is that second half, and
// the only place it is spelled.
export const sizeText = (mib: number | undefined): string => driveSizeLabel(mib) ?? DRIVES.SIZE_NONE;

// The enforcement gloss that rides every rendered size, so the honesty is on
// each number and not only in the HONESTY note. Total over the vocabulary —
// including `filesystem`, which no v1 backend yields (§2.7).
const GLOSS: Record<StorageEnforcement, string> = {
  filesystem: DRIVES.ENFORCEMENT_FILESYSTEM,
  request: DRIVES.ENFORCEMENT_REQUEST,
  external: DRIVES.ENFORCEMENT_EXTERNAL,
  none: DRIVES.ENFORCEMENT_NONE,
};

export const enforcementGloss = (e: StorageEnforcement | undefined): string =>
  GLOSS[e ?? "none"] ?? DRIVES.ENFORCEMENT_NONE;

// Mode is a fact-chip with a WORD, never colour alone. Writable is amber
// because it is a widened blast radius (the same reason /permissions paints an
// allow amber); read-only is neutral.
export const modeText = (writable: boolean | undefined): string =>
  writable ? DRIVES.MODE_RW : DRIVES.MODE_RO;
export const modeTone = (writable: boolean | undefined): "warning" | "neutral" =>
  writable ? "warning" : "neutral";
