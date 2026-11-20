/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of the change review: the Effect column of the Changes queue and the verdict, impact, risk and raw
// diff sections of a change. Plain strings and small functions, no React, so the components and the helpers both
// read them from here.
//
// Strings marked "not on the board" are wording the approved board does not draw.
import type { HeldBecause, RiskLevel } from "./types";

const plural = (n: number, one: string, many: string) => (n === 1 ? one : many);

export const CHANGE_REVIEW = {
  // The queue's column.
  COL_EFFECT: "Effect",

  VERDICT: {
    widens: "Widens",
    narrows_held: (because: string) => `Narrows, but held because ${because}`,
    not_checked: "Not checked for this kind",
  },
  HELD_BECAUSE: {
    contact: "the contact changes",
    lock_removed: "a lock is removed",
    rename: "a rename may change who gets which profile",
    /** not on the board. */
    legacy_shift: "a move changes whose older assignment applies",
    /** not on the board. */
    first_allowlist: "it sets the first allowlist, with no earlier one to judge it against",
  } as Record<HeldBecause, string>,
  NOT_CHECKED_NOTE: (kind: string) => `Treated as widening: no narrowing check exists for ${kind} changes.`,

  RISK_CHIP: (before: string, after: string) => `Risk ${before} → ${after}`,
  /** not on the board: a profile create has no earlier grade. */
  RISK_CHIP_NEW: (after: string) => `Risk ${after}`,
  RISK_LEVEL: { low: "Low", medium: "Medium", high: "High" } as Record<RiskLevel, string>,

  SENTENCE_HOSTS: (target: string, n: number) =>
    `This lets runs under ${target} reach ${n} new ${plural(n, "host", "hosts")}.`,
  /** not on the board. */
  SENTENCE_WIDENS: (target: string, labels: string) => `This widens ${labels} for runs under ${target}.`,
  /** The board's wording names a raised barrier floor; the direction word is not drawn for each field. */
  SENTENCE_ALSO: (label: string, before: string, after: string) => `It also changes ${label} from ${before} to ${after}.`,
  ADDS: "Adds",
  ALSO_CHANGES: "Also changes:",
  RULES_NOTE: "Written by Wardyn's rules from the server's diff, not by a model. The same change always reads the same way.",

  IMPACT: "Impact",
  AS_OF_NOW: "As of now",
  PEOPLE: "People",
  UNITS: "units",
  LIVE_RUNS_WALLS: "live runs whose walls change",
  NOT_IN_REPORT: "Not in this kind's report",
  STORED_POLICIES: "stored policies whose walls change",
  WHOSE_WALLS: "Whose walls change",
  COL_PERSON: "Person",
  COL_UNIT: "Unit",
  COL_WHAT_CHANGES: "What changes for them",
  MORE: (n: number) => `+ ${n} more`,
  LIVE_RUNS: "Live runs",
  COL_RUN: "Run",
  COL_RUN_OWNER: "Run owner",
  COL_STARTED: "Started",
  IMPACT_NOT_COMPUTED: (kind: string) => `Impact is not computed for ${kind} changes yet.`,
  /** not on the board. */
  IMPACT_STALE: (target: string) =>
    `${target} changed after this change was proposed, so approving it would be refused as stale.`,
  /** not on the board. */
  IMPACT_REFUSED: "Impact can't be computed for you on this change.",

  RISK: "Risk",
  RISK_RULES_NOTE: "Graded by Wardyn's rules, never by a model.",
  RISK_TODAY: "Today",
  RISK_AFTER: "After approval",
  RISK_NEW_OR_CHANGED: "New or changed",
  RISK_NOT_GRADED: (kind: string) => `Risk is graded for policy-shaped kinds only. ${kind} changes have no grade.`,

  RAW_DIFF: "Raw diff",
  SHOW_FULL: "Show full before and after",
} as const;
