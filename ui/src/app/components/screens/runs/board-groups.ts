/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runs board's pure logic — grouping, headlines, and the ONE attention
// rule. No JSX, so App.tsx's sidebar badge imports the same rule the board
// renders: those used to be two hand-copied Sets of run states that could
// (and did) disagree about what "needs you" means.
//
// Attention is no longer a function of the run state alone. A held approval
// parks the sandbox while the run is still RUNNING, so the state says nothing
// about it — the signal comes from the PENDING approvals list, joined here.
import type { AgentRun, ApprovalRequest } from "../../../lib/types";
import { isHeld } from "../../wardyn/live-approvals";
import {
  attentionFor,
  attentionRank,
  type AttentionSignals,
  type RunAttention,
} from "../../wardyn/run-state-glyph";

// Ranks 1-3 (permission / interrupted / monitoring) are the "needs you" band —
// see run-state-glyph.tsx's RANK, which is the single ordering.
const ATTENTION_RANK = 3;

/** One run's live approval facts, as the glyph's signals plus the count the
 *  card states ("2 waiting · sandbox held"). */
interface RunApprovalSignals extends AttentionSignals {
  pending: number;
}

export type RunSignals = ReadonlyMap<string, RunApprovalSignals>;

/**
 * Held vs passive, per run, from ONE PENDING approvals fetch.
 *
 * `isHeld` is imported rather than re-derived: the run cockpit already states
 * this exact fact one screen over, and two copies of the wait_for_review test
 * would be two truths that can disagree — with the disagreement reading as
 * "nothing is holding the sandbox" while the sandbox is, in fact, held.
 *
 * ponytail: that import costs ~3.4kB gzip on the ENTRY chunk. This module is
 * eager (the board is the landing route, and App.tsx's badge shares the rule),
 * live-approvals.tsx is otherwise lazy under run-detail, and Rollup assigns
 * chunks per MODULE — so a shared import hoists the whole file up rather than
 * just this predicate. Correctness bought the bytes. To get them back, move
 * isHeld into lib/types/approvals.ts beside decisionArgs (which lives there
 * for a comparable reason) and have both callers import it from there.
 */
export function approvalSignals(pending: readonly ApprovalRequest[]): RunSignals {
  const by = new Map<string, RunApprovalSignals>();
  for (const a of pending) {
    if (a.state !== "PENDING") continue;
    const cur = by.get(a.run_id) ?? { pending: 0 };
    cur.pending += 1;
    // A held request blocks; a passive deny_with_review pending does not. Once
    // anything on the run is held, the run is held — a passive sibling can
    // never downgrade that.
    if (isHeld(a)) cur.held = true;
    else cur.passiveHold = true;
    by.set(a.run_id, cur);
  }
  return by;
}

export function signalsFor(run: AgentRun, signals: RunSignals): RunApprovalSignals {
  return signals.get(run.id) ?? { pending: 0 };
}

export function runAttention(run: AgentRun, signals: RunSignals): RunAttention {
  return attentionFor(run.state, signalsFor(run, signals));
}

/** The amber-badge rule, shared by the sidebar count and the board. */
export function needsAttention(run: AgentRun, signals: RunSignals): boolean {
  return attentionRank(runAttention(run, signals)) <= ATTENTION_RANK;
}

/**
 * Pinned-lane membership: an approval is a REQUEST and a failure is a REPORT
 * (CONSOLE-RULES §5's precedence note). Only the request pins — rank 1. A
 * failed or killed run keeps its place beside the work it belongs to and
 * carries a danger rail there instead.
 */
export function needsYou(run: AgentRun, signals: RunSignals): boolean {
  return runAttention(run, signals) === "permission";
}

// ── title grouping ──────────────────────────────────────────────────────────
// The board groups by the run's TITLE: runs that share one are the same piece
// of work, and seeing the twelve nightly audits as one thing is the point of
// naming them. It replaced grouping by state — the triage that gave up is
// preserved two ways: the state facet still narrows BEFORE grouping, and the
// input list arrives pre-ordered attention → active → done, so a group holding
// a failed run floats to the top for free and its header says so.
//
// A title held by only ONE run is not a group. Those, and every untitled run
// (legacy rows, CLI runs, the server's own system runs), fall into a single
// trailing grid — a header per singleton is noise, not structure.
//
// ponytail: computed over the LOADED page, not the server. A title split across
// a pagination boundary groups per page; add a server-side group-by if run
// counts ever outgrow LIST_LIMIT.
export function titleGroups(runs: AgentRun[]): {
  groups: { title: string; runs: AgentRun[] }[];
  loose: AgentRun[];
} {
  const by = new Map<string, AgentRun[]>();
  for (const r of runs) {
    const t = (r.title ?? "").trim();
    if (t) by.set(t, [...(by.get(t) ?? []), r]);
  }
  const groups = [...by].filter(([, rs]) => rs.length > 1).map(([title, rs]) => ({ title, runs: rs }));
  const grouped = new Set(groups.flatMap((g) => g.runs.map((r) => r.id)));
  return { groups, loose: runs.filter((r) => !grouped.has(r.id)) };
}

// The last rung of the headline chain, and the repo slot's stand-in — both
// name what the run actually is rather than rendering a blank or a dash.
// "Ephemeral scratch — no repo" is the New Run screen's own workspace string
// (new-run-screen.tsx), so the board and the form agree on what "no workspace"
// is called.
export const INTERACTIVE_HEADLINE = "Interactive session";
export const NO_REPO = "Ephemeral scratch — no repo";

function basename(path: string | undefined): string {
  const trimmed = (path ?? "").replace(/\/+$/, "");
  return trimmed.split("/").pop() ?? "";
}

/**
 * What one card/row calls itself, with honest fallbacks all the way down.
 *
 * INSIDE a group the title is already on the header, so the row's job is to
 * distinguish this run from its siblings — the task does that; the title would
 * print three identical cards.
 *
 * Nothing here is invented. Each rung is a fact the run carries, and
 * "Interactive session" is claimed only for a run that IS one — a nameless
 * non-interactive run still gets the dash rather than a flattering guess.
 */
export function rowHeadline(run: AgentRun, grouped: boolean): string {
  const named = grouped ? "" : (run.title ?? "").trim();
  return (
    named ||
    run.task ||
    basename(run.workspace_path) ||
    (run.interactive ? INTERACTIVE_HEADLINE : "—")
  );
}

/** Row 2's repo cluster: the repo, else the workspace it mounted, else the
 *  honest note that there is neither. Only a real path gets mono type —
 *  the fallback is a phrase, and §3 keeps mono for literals. */
export function repoLabel(run: AgentRun): { text: string; mono: boolean } {
  const text = run.repo || run.workspace_path || "";
  return text ? { text, mono: true } : { text: NO_REPO, mono: false };
}

export function shortId(id: string): string {
  const base = id.replace(/^run_/, "");
  return base.length > 10 ? base.slice(0, 8) + "…" : base;
}
