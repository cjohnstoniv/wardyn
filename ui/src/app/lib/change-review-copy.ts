/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of the change review: the Effect column of the Changes queue and the verdict, impact, risk and raw
// diff sections of a change. Plain strings and small functions, no React, so the components and the helpers both
// read them from here.
//
// Strings marked "not on the board" are wording the approved board does not draw.
import type { ChangeVerdict, HeldBecause, RiskLevel } from "./types";

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
  /** Proposed: says only what this stored change lacks, since the server cannot tell a kind with no check from a change whose check gave no result. */
  NOT_CHECKED_NOTE: "No narrowing result is available for this change. Treat it as widening.",

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
  /** Proposed: a widened blocked-hosts rule lists the blocked hosts that are no longer blocked. */
  REMOVES_FROM_BLOCKED: "Removes from blocked hosts",
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
  /** Proposed: record-scoped for the same reason as NOT_CHECKED_NOTE. */
  RISK_NOT_GRADED: "No risk grade is available for this change.",

  RAW_DIFF: "Raw diff",
  SHOW_FULL: "Show full before and after",
} as const;

// The structured fields a review sentence is written from: the widened fields (their plain name, or the field's
// own name when it has none) and the other fields the change moves, each with the value on both sides.
export interface ReviewSentenceInput {
  verdict: ChangeVerdict | undefined;
  target: string;
  widens: { field: string; label: string; entries?: string[] }[];
  also: { label: string; before: string; after: string }[];
}

// "a, b and c".
const joinAnd = (items: string[]) =>
  items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;

// The one plain sentence for a widening change, from the server's structured review: the new hosts when the allowed
// hosts widen, the widened fields' names for any other widening, then up to two other changes. Null for a change
// that does not widen. This is the only place the sentence is written, so every surface reads the same words.
export function renderReviewSentence(input: ReviewSentenceInput): string | null {
  if (input.verdict !== "widens") return null;
  const sentences: string[] = [];
  const hosts = input.widens.find((w) => w.field === "allowed_domains" && (w.entries?.length ?? 0) > 0);
  if (hosts) sentences.push(CHANGE_REVIEW.SENTENCE_HOSTS(input.target, hosts.entries!.length));
  const others = input.widens.filter((w) => w !== hosts).map((w) => w.label);
  if (others.length > 0) sentences.push(CHANGE_REVIEW.SENTENCE_WIDENS(input.target, joinAnd([...new Set(others)])));
  for (const a of input.also.slice(0, 2)) sentences.push(CHANGE_REVIEW.SENTENCE_ALSO(a.label, a.before, a.after));
  return sentences.length > 0 ? sentences.join(" ") : null;
}
