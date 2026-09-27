/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Copy for the compact TierPicker (#1200) and the governance profile editor's
// "Allowed barriers" control it introduces. NEW canon, not a §7.2-§7.13 row —
// governance-copy.ts's own doc-parity suite (governance-copy.test.ts) parses
// docs/design/governance-prompt.md's frozen tables and would gain nothing by
// hosting strings that doc never named, so this stays a separate module
// rather than a silent addition to GOVERNANCE.
//
// Every reused string (CC_META's label/tagline/mechanism/protects/doesntProtect,
// RESIDUAL_PREFIX, BTN.showSetupCommand, CONFINEMENT_CONSTANT_NOTE, the
// min_confinement_class field help) is imported and rendered verbatim by
// tier-picker.tsx, never retyped here — this module owns only what's
// genuinely NEW: the picker's own instruction line, the decided-state line,
// and the Allowed-barriers editor's label + three summary sentences
// (tier-picker-1200-packet.html's Strings table).
import type { ConfinementClass } from "./types";

export const TIER_PICKER = {
  // Was "Weakest to strongest — pick a column to save it in this browser as
  // the default barrier for new runs." (environment-step.tsx, unchanged
  // there — the full table is the one control with actual columns). T-4:
  // every non-table shape says "pick one" instead.
  PICK_ONE:
    "Weakest to strongest — pick one to save it in this browser as the default barrier for new runs.",
  // #1200 review P2-3 — New Run's own pick is per-run and nothing persists
  // (no localStorage write anywhere under new-run/*), so PICK_ONE's browser-
  // persistence claim would be false here.
  PICK_ONE_PER_RUN: "Weakest to strongest — pick one for this run.",
  // Any user picker collapsed to its one allowed+installed tier (Strings
  // table: "any user picker with exactly one allowed+installed tier").
  DECIDED: (tierLabel: string) => `${tierLabel} · set by your admin`,
  COMPARE_BARRIERS: "Compare barriers",
  COMPARE_BARRIERS_TITLE: "Compare barriers",
  READ_THE_DOCS: "Read the docs",
  READ_THE_DOCS_URL: "https://github.com/cjohnstoniv/wardyn/blob/main/docs/OPERATIONS.md",
  ABOUT: (tierLabel: string) => `About ${tierLabel}`,
  // T-9 — the existing no-runner/incompatible danger card, plus one line
  // naming the requirement. `tierLabel` is the profile's required tier;
  // `reason` is the same honest reason environment-step.tsx already computes
  // for that tier (a KVM absence, a missing runtime).
  REQUIREMENT_TITLE: "No sandbox runner — runs can't launch.",
  // Generic on purpose: the floor a caller passes in may come from an admin's
  // governance ceiling, this run's own authored policy, or whichever of the
  // two ranks higher (combineFloors, new-run/policy-lane.ts) — the sentence
  // must be true under all three, so it never claims a specific source.
  REQUIREMENT_LINE: (tierLabel: string, reason: string) =>
    `This run's floor requires ${tierLabel}, and this host can't run it: ${reason}`,
  // The governance-specific variant (Host card / member Getting-started
  // contexts where the ONLY source of a floor is an assigned profile).
  GOVERNANCE_REQUIREMENT_LINE: (tierLabel: string, reason: string) =>
    `Your admin requires ${tierLabel}, and this host can't run it: ${reason}`,
  // display mode (Host card, member Getting-started), zero tiers to list.
  NONE_INSTALLED: "No barrier is installed on this host yet.",

  // ---- Governance profile editor · Allowed barriers (§3a) ----
  ALLOWED_BARRIERS_LABEL: "Allowed barriers",
  ALLOWED_BARRIERS_SUMMARY: (floor: ConfinementClass): string =>
    ({
      CC1: "Fence, Wall and Vault are all allowed under this profile.",
      CC2: "Wall and Vault are allowed under this profile; Fence is not.",
      CC3: "Only Vault is allowed under this profile — every run is forced onto it.",
    })[floor],
};
