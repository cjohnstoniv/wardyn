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
import { DRIVES } from "../../../lib/user-drives-copy";
import { driveSizeLabel } from "../../../lib/user-drives-display";
import type { StorageEnforcement } from "../../../lib/api/drives";
import { makeMono } from "../../wardyn/code-block";

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
export const withMono = makeMono(["WARDYN_USER_DRIVE_HOST_ROOTS", "/home/agent/drive", "disk_mib", ". _ -"]);

// The admin's spelling of a size: THE one size helper (lib/user-drives-display
// .ts's driveSizeLabel, §5 #10 — never lib/format.ts's fmtBytes), with the
// admin half of the state it deliberately refuses to choose. driveSizeLabel
// returns null for 0/absent because a member's sentence takes the _NOSIZE twin
// there while an admin's cell renders SIZE_NONE; this is that second half, and
// the only place it is spelled.
export const sizeText = (mib: number | undefined): string => driveSizeLabel(mib) ?? DRIVES.SIZE_NONE;

// The enforcement gloss that rides every rendered size, so the honesty is on
// each number and not only in the HONESTY note. Total over the vocabulary —
// including `filesystem`, which no v1 backend yields (§2.7). `eviction`
// (0.7.2) is Kubernetes' ephemeral-scratch word; the Workspace Providers
// Storage tab reads this SAME map (workspace-providers-prompt.md §7.3: "the
// enforcement words are not re-frozen here") rather than a second gloss.
export const GLOSS: Record<StorageEnforcement, string> = {
  filesystem: DRIVES.ENFORCEMENT_FILESYSTEM,
  request: DRIVES.ENFORCEMENT_REQUEST,
  external: DRIVES.ENFORCEMENT_EXTERNAL,
  none: DRIVES.ENFORCEMENT_NONE,
  eviction: DRIVES.ENFORCEMENT_EVICTION,
};

// ABSENT IS UNKNOWN, NOT `none`, and that is the half that was wrong. An older
// daemon or an undetected runner sends no word at all, and folding that to
// "none" printed "nothing binds this size" — a positive claim about the
// substrate — under all three disk fields, while isUncappedEnforcement below
// deliberately withheld the matching warning for the same input. One of the two
// had to be total, and the honest one is silence: no word, no gloss.
export const enforcementGloss = (e: StorageEnforcement | undefined): string =>
  e === undefined ? "" : (GLOSS[e] ?? DRIVES.ENFORCEMENT_NONE);

// Does a number typed against this word run UNCAPPED? Only `none` does: the
// substrate binds nothing, so a filled/clamped size runs with a warning on the
// run and a policy-authored size fails at create — the three clauses of
// PROVIDERS.DOCKER_UNCAPPED_WARN. Every other word binds bytes somewhere:
// `filesystem` refuses the write, `eviction` (k8s) kills the pod over the
// limit, `request`/`external` bind at the volume. Absent (older daemon, no
// runner detected) is UNKNOWN and yields no warning — the same input
// enforcementGloss above now renders no gloss for, so the two halves agree
// instead of one of them asserting `none` while the other withholds it. The two disk surfaces
// (/providers Storage tab, the governance profile editor's ephemeral row) BOTH
// read this predicate so they cannot drift apart.
export const isUncappedEnforcement = (e: StorageEnforcement | undefined): boolean => e === "none";

// Mode is a fact-chip with a WORD, never colour alone. Writable is amber
// because it is a widened blast radius (the same reason /permissions paints an
// allow amber); read-only is neutral.
export const modeText = (writable: boolean | undefined): string =>
  writable ? DRIVES.MODE_RW : DRIVES.MODE_RO;
export const modeTone = (writable: boolean | undefined): "warning" | "neutral" =>
  writable ? "warning" : "neutral";
