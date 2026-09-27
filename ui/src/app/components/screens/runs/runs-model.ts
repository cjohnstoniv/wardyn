/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — the D2 row's pure presentation rule and the page's section
// bucketing. No JSX (so it is cheap to exhaustively vitest) and no fetch: it
// takes the already-server-attention-projected AgentRun[] one GET /runs?view=
// call returns and decides what to render.
//
// The attention field (run.attention, internal/types/attention.go's
// RunAttention, projected server-side per #1197 L1b) is the ONE source of
// truth for "who is waiting on what" — this file never re-derives a hold from
// approvals the way the old board-groups.ts did. See design.md §2.2 for the
// row table this mirrors, and FINAL-PR-1230.md's Q1/Q5 for why `by` already
// encodes the ADO-ceiling nuance ("Waiting for an admin") without this file
// needing to know about ceilings at all.
import type { AgentRun, RunState } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { waitingAdoConsent, waitingReauth } from "../../../lib/reauth-waiting-copy";
import { RUN_WAIT } from "../../wardyn/copy/run-wait";
import { RUNS_ROW_WORD } from "../../wardyn/copy/runs-landing";

export type RowHue = "blue" | "amber" | "red" | "grey";
export type RowAction = "review" | "sign-in" | null;

export interface RowPresentation {
  hue: RowHue;
  word: string;
  /** The held-count subline ("1 waiting · sandbox held") — approval kind only. */
  subword?: string;
  action: RowAction;
  /** Whether THIS viewer can clear the hold — the section-placement fact. */
  needsYou: boolean;
}

const LIVE_STATES: ReadonlySet<RunState> = new Set([
  "PENDING",
  "STARTING",
  "RUNNING",
  "WAITING_FOR_CONFIRMATION",
]);

export function isLiveRunState(state: RunState): boolean {
  return LIVE_STATES.has(state);
}

/**
 * The row's presentation, given its server-projected attention (if any) and
 * its state. Word/hue/action never re-derive `by` — the server already
 * resolved "can THIS viewer act", including the ADO-ceiling gap FINAL-PR-1230
 * documents (mayDecide answers "may act (approve or at least deny)"; the row
 * offers Review either way, and the decision card underneath is where the
 * real ceiling is enforced — same as today).
 */
export function rowPresentation(run: AgentRun, adminView: boolean): RowPresentation {
  const a = run.attention;
  if (a) {
    const you = a.by === "you";
    switch (a.kind) {
      case "approval":
        return {
          hue: "amber",
          word: you
            ? adminView
              ? RUNS_ROW_WORD.NEEDS_DECISION
              : RUNS_ROW_WORD.NEEDS_APPROVAL
            : RUNS_ROW_WORD.WAITING_FOR_ADMIN,
          subword: RUN_WAIT.waitingHeld(a.pending),
          action: you ? "review" : null,
          needsYou: you,
        };
      case "reauth":
        return {
          hue: "amber",
          word: waitingReauth(a.pending, you),
          action: you ? "sign-in" : null,
          needsYou: you,
        };
      case "ado_consent":
        return {
          hue: "amber",
          word: waitingAdoConsent(a.pending, you),
          action: you ? "sign-in" : null,
          needsYou: you,
        };
      case "lost":
        // #1197 L5 owns the Revive row and its own copy — this is a clean
        // seam (H-7): the run still lands in Needs you when `you`, with the
        // reused short word and NO action, rather than nothing at all.
        return { hue: "amber", word: RUNS_ROW_WORD.SANDBOX_STOPPED, action: null, needsYou: you };
    }
  }
  switch (run.state) {
    case "RUNNING":
      return { hue: "blue", word: RUNS_ROW_WORD.RUNNING, action: null, needsYou: false };
    case "STARTING":
      return { hue: "blue", word: RUNS_ROW_WORD.STARTING, action: null, needsYou: false };
    case "PENDING":
      return { hue: "blue", word: RUNS_ROW_WORD.QUEUED, action: null, needsYou: false };
    case "WAITING_FOR_CONFIRMATION":
      // Row 4 (run_attention.go) — "reserved, no producer today": a state
      // reaching here with no attention projected is not currently possible,
      // but degrade to the live word rather than the terminal grey below.
      return { hue: "blue", word: RUNS_ROW_WORD.RUNNING, action: null, needsYou: false };
    case "COMPLETED":
      return { hue: "grey", word: "Completed", action: null, needsYou: false };
    case "FAILED":
      return { hue: "red", word: "Failed", action: null, needsYou: false };
    case "KILLED":
      return { hue: "grey", word: "Killed", action: null, needsYou: false };
    case "STOPPED":
      return { hue: "grey", word: "Stopped", action: null, needsYou: false };
    case "ARCHIVED":
      return { hue: "grey", word: "Archived", action: null, needsYou: false };
    default:
      return { hue: "grey", word: String(run.state), action: null, needsYou: false };
  }
}

export interface RunSections {
  decide: AgentRun[];
  // Admin view only (H-3): an owner's own sign-in hold or lost run — nobody
  // but the owner can act, so this section never carries a button (rowPresentation
  // already answers `action: null` for by=owner; this is only the section
  // placement half of that same fact).
  waitingOwner: AgentRun[];
  running: AgentRun[];
  endedToday: AgentRun[];
  earlier: AgentRun[];
  older: { label: string; runs: AgentRun[] }[];
}

// Which of the two attention sections (if either) a run belongs in. `by ===
// "admin"` (H-3a: a member's run held on an admin-only approval) answers
// null here — it stays in the ordinary running/ended flow, unchanged from L3.
function attentionSection(run: AgentRun): "decide" | "waitingOwner" | null {
  const by = run.attention?.by;
  if (by === "you") return "decide";
  if (by === "owner") return "waitingOwner";
  return null;
}

// The flat, non-attention run list — running/starting/ended, in the server's
// own order — that "Group by Workspace/Title" (H-6, runs-groups.ts) buckets
// instead of the fixed time sections. Needs-you and Waiting-on-the-owner
// always render as their own sections regardless of the group-by option
// (design.md §6's own mock: only the "everything else" portion regroups).
export function nonAttentionRuns(runs: readonly AgentRun[]): AgentRun[] {
  return runs.filter((r) => attentionSection(r) === null);
}

function endedAtMs(run: AgentRun): number {
  return Date.parse(run.ended_at ?? run.updated_at ?? run.created_at);
}

function isSameCalendarDay(aMs: number, bMs: number): boolean {
  const a = new Date(aMs);
  const b = new Date(bMs);
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/**
 * The page's fixed section order (design.md §1/§6 H-1/H-6): needs, then
 * running, then ended-by-recency. `now` is injectable for tests.
 *
 * Grouping stops at "Earlier this week": anything older only appears once a
 * filter widens the ended-within window past 7 days, bucketed by day
 * (Earlier this month / Older) — the fixed L3 scope; per-workspace/per-title
 * grouping is L4 (design.md §6 build plan).
 */
export function sectionRuns(runs: readonly AgentRun[], now: number = Date.now()): RunSections {
  const decide: AgentRun[] = [];
  const waitingOwner: AgentRun[] = [];
  const running: AgentRun[] = [];
  const over: AgentRun[] = [];
  for (const run of runs) {
    const section = attentionSection(run);
    if (section === "decide") {
      decide.push(run);
    } else if (section === "waitingOwner") {
      waitingOwner.push(run);
    } else if (!isTerminalRunState(run.state)) {
      // Includes a lease-ended run (lost_reason "ended") — it stays RUNNING
      // until the ended-run grace, and its own "Ended at its end time" grey
      // placement is L5 scope (design.md §4/§6); this is that seam.
      running.push(run);
    } else {
      over.push(run);
    }
  }
  const endedToday: AgentRun[] = [];
  const earlier: AgentRun[] = [];
  const month: AgentRun[] = [];
  const older: AgentRun[] = [];
  const DAY_MS = 24 * 60 * 60 * 1000;
  for (const run of over) {
    const ms = endedAtMs(run);
    const ageMs = now - ms;
    if (isSameCalendarDay(ms, now)) endedToday.push(run);
    else if (ageMs <= 7 * DAY_MS) earlier.push(run);
    else if (ageMs <= 30 * DAY_MS) month.push(run);
    else older.push(run);
  }
  const olderBuckets: { label: string; runs: AgentRun[] }[] = [];
  if (month.length) olderBuckets.push({ label: "Earlier this month", runs: month });
  if (older.length) olderBuckets.push({ label: "Older", runs: older });
  return { decide, waitingOwner, running, endedToday, earlier, older: olderBuckets };
}

// "All quiet" (design.md §2.1) is a fact about the TOP of the page only —
// nothing needs you and nothing is running. Ended sections still render
// below the dashed line, so this deliberately ignores endedToday/earlier/older.
// waitingOwner counts too (Admin view): a page with only owner sign-in holds
// showing is not quiet, even though nothing on it is THIS viewer's to clear.
export function isTopQuiet(sections: RunSections): boolean {
  return sections.decide.length === 0 && sections.waitingOwner.length === 0 && sections.running.length === 0;
}
