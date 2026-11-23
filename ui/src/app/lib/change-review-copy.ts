/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words of the change review: the Effect column of the Changes queue and the verdict, impact, risk and raw
// diff sections of a change. Plain strings and small functions, no React, so the components and the helpers both
// read them from here.
//
// Strings marked "not on the board" are wording the approved board does not draw.
import type { ApprovalDropped, ApproverTier, ChangeVerdict, HeldBecause, RiskLevel } from "./types";

const plural = (n: number, one: string, many: string) => (n === 1 ? one : many);

export const CHANGE_REVIEW = {
  // The queue's column.
  COL_EFFECT: "Effect",

  VERDICT: {
    widens: "Widens",
    narrows_held: (because: string) => `Narrows, but held because ${because}`,
    /** Proposed: the board says "for this kind", but the server cannot tell a kind with no check from a change whose check gave no result. */
    not_checked: "Not checked",
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
  /** Proposed: the words before a widening's entries, one per field, for the direction its entries mean. */
  NO_LONGER_WITHHELD: "No longer withheld:",
  NO_LONGER_BOUND: "No longer bound:",
  LOOSENED: "Loosened:",
  RAISED: "Raised:",
  MOVES: "Moves:",
  AFFECTED: "Affected:",
  ALSO_CHANGES: "Also changes:",
  RULES_NOTE: "Written by Wardyn's rules from the server's diff, not by a model. The same change always reads the same way.",

  IMPACT: "Impact",
  AS_OF_NOW: "As of now",
  /** The report counts assignments (memberships), one per subject; an Everyone assignment is one row for many people. */
  ASSIGNMENTS: "Assignments affected",
  UNITS: "units",
  LIVE_RUNS_WALLS: "live runs whose walls change",
  NOT_IN_REPORT: "Not in this kind's report",
  STORED_POLICIES: "stored policies whose walls change",
  WHOSE_WALLS: "Whose walls change",
  COL_ASSIGNED_TO: "Assigned to",
  SUBJECT_PERSON: "Person",
  SUBJECT_GROUP: "Group",
  /** An assignment with no subject covers everyone the unit's assignment reaches. */
  SUBJECT_EVERYONE: "Everyone",
  COL_UNIT: "Unit",
  COL_WHAT_CHANGES: "What changes for them",
  MORE: (n: number) => `+ ${n} more`,
  LIVE_RUNS: "Live runs",
  COL_RUN: "Run",
  COL_RUN_OWNER: "Run owner",
  COL_STARTED: "Started",
  /** Proposed: scoped to this change, since the server computes some operations of a kind and not others. */
  IMPACT_NOT_COMPUTED: "Impact is not available for this change.",
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

// The words before a widening's entries. Only the allow-type fields add entries; the others list what a rule
// stops doing or by how much it loosens, so "Adds" would reverse their meaning. A field this console does not
// know gets the neutral "Affected:".
export function entriesLabel(field: string): string {
  switch (field) {
    case "allowed_domains":
    case "allowed_methods":
    case "eligible_grants":
      return CHANGE_REVIEW.ADDS;
    case "denied_domains":
      return CHANGE_REVIEW.REMOVES_FROM_BLOCKED;
    case "withheld_grants":
      return CHANGE_REVIEW.NO_LONGER_WITHHELD;
    case "members":
      return CHANGE_REVIEW.NO_LONGER_BOUND;
    case "vcs":
      return CHANGE_REVIEW.LOOSENED;
    case "member_cap":
      return CHANGE_REVIEW.RAISED;
    case "owning_unit":
      return CHANGE_REVIEW.MOVES;
    default:
      return CHANGE_REVIEW.AFFECTED;
  }
}

// The words of a change's own page: its header, the approvals panel and the decision footer. The panel counts
// nothing itself; every number in these sentences comes from the server's approvals block.
//
// Strings marked "not on the board" are wording the approved board does not draw.
export const CHANGE_APPROVALS = {
  /** "Governance › Changes", a link back to the queue. */
  BREADCRUMB: (governance: string, changes: string) => `${governance} › ${changes}`,
  TAB_EFFECT: "Effect",
  /** Names the tab strip for assistive technology; not on the board. */
  TABS_LABEL: "Sections of this change",
  /** Shown in place of the page while the change is read; not on the board. */
  LOADING: "Loading this change",
  /** Shown over a change that is no longer waiting; not on the board. */
  DECIDED: { applied: "This change was applied.", rejected: "This change was rejected.", expired: "This change expired." } as Record<string, string>,
  /** The counts row under the verdict; not on the board. */
  IMPACT_SUMMARY: (units: number, runs: number) =>
    `${units} ${plural(units, "unit", CHANGE_REVIEW.UNITS)} · ${runs} ${plural(runs, "live run", "live runs")} whose walls change`,

  TITLE: "Approvals",
  PROGRESS: (have: number, required: number) => `${have} of ${required} approved`,
  WAITING: (have: number, required: number) => `Waiting: ${have} of ${required}`,
  ONE_PERSON: "One person decides",

  /** The rule line. A required 1 is not on the board; with the security rule on, the clause stays. */
  RULE: (required: number, security: boolean) => {
    if (required <= 1) return security ? "Needs 1 approval, from a security admin. The proposer never counts." : "Needs 1 approval. The proposer never counts.";
    return security
      ? `Needs ${required} approvals, at least one from a security admin. The proposer never counts.`
      : `Needs ${required} approvals. The proposer never counts.`;
  },
  TIER: {
    security_admin: "Security admin",
    super_admin: "Admin",
    unit_admin: "Unit admin",
  } as Record<ApproverTier, string>,
  /** A unit admin names the unit when the server does. */
  TIER_UNIT: (unit: string) => `unit admin of ${unit}`,
  APPROVED_ENTRY: (approver: string, when: string, tier: string) => `${approver} approved ${when} · ${tier}`,
  /** The proposer's line: they started the change and never count toward it. */
  PROPOSER: (who: string) => `${who} proposed this and doesn't count`,
  DROPPED: (approver: string, why: string, at?: string) =>
    `${approver}'s approval no longer counts: ${why}${at ? ` at ${at}` : ""}.`,
  WHY_DROPPED: {
    role_removed: (tier: string) => `their ${tier} role was removed`,
    /** not on the board. */
    deactivated: () => "they were deactivated",
    /** not on the board. */
    token_revoked: () => "the token they approved with was revoked",
    /** not on the board. */
    session_revoked: () => "their sessions were revoked after they approved",
    /** not on the board. */
    grant_removed: () => "their unit admin grant no longer covers this change",
    /** not on the board. */
    cant_confirm: () => "their role can't be confirmed",
  } as Record<ApprovalDropped, (tier: string) => string>,

  CAN_APPROVE: "Can approve",
  ELIGIBLE: (who: string, tier: string) => `${who} · ${tier}`,
  /** not on the board. A line that opens the list has no leading "and". */
  UNCONFIRMED: (n: number, first: boolean) => `${first ? "" : "and "}${n} ${plural(n, "person", "people")} whose role can't be confirmed`,
  /** not on the board. */
  TOO_MANY: (first: boolean) => (first ? "More people than Wardyn counts" : "and more people than Wardyn counts"),
  /** not on the board. */
  NEVER_SIGNED_IN: "People who have never signed in can't approve until they do.",

  /** The final clause shows to the proposer only. */
  CANT_REACH_COUNT: (required: number, still: number, remaining: number, expires: string, proposer: boolean) =>
    `This change can't reach ${required} approvals: it still needs ${still}, and only ${remaining} more can approve it. It expires ${expires} unless someone is given the right to approve it${proposer ? ", or you withdraw it." : "."}`,
  /** Proposed: carries no number, since the blocker is the missing security admin. */
  CANT_REACH_SECURITY: (expires: string, proposer: boolean) =>
    `This change can't be approved: it needs a security admin, and no one who can still approve it is one. It expires ${expires} unless someone is given the right to approve it${proposer ? ", or you withdraw it." : "."}`,
  /** not on the board. */
  CANT_CONFIRM: (required: number) =>
    `Wardyn can't confirm this change can reach ${required} approvals: some people who could approve it can't be counted.`,
  CANNOT_BE_LAST: "Your approval can't be the last one: one must come from a security admin.",

  REASON_REQUIRED_LABEL: "Reason (required to reject)",
  STATUS_COUNTS: (n: number, required: number) => `Your approval counts ${n} of ${required}.`,
  STATUS_LAST: "Yours is the last approval needed. The change applies when you approve.",
  STATUS_APPROVED: (when: string, still: number) => `You approved ${when}. Waiting for ${still} more.`,
  TAKE_BACK: "Take back my approval",
  WITHDRAW: "Withdraw",
  REJECT_NOTE: "Rejecting ends this change for everyone. It doesn't apply and its approvals are dropped.",
  REASON_NEEDED: "Write a reason to reject. It is kept with the change.",
  STALE: (target: string, proposer: string) =>
    `This change is stale: ${target} changed after it was proposed. It can't apply, and its approvals are void. Ask ${proposer} to propose it again.`,
  /** The toast after a take-back; not on the board. */
  TOAST_TAKEN_BACK: "Approval taken back",
} as const;
