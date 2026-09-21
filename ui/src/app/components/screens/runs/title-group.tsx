/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One title's runs: the board's grouping unit now that runs are named.
//
// The header carries per-state counts, which is what makes replacing the old
// Needs-attention / Active / Done sections honest — the triage those sections
// provided is still legible here, per group, instead of splitting one piece of
// work across three places on the page. #160 adds a second row: one counted
// chip per reason the group's runs are waiting, so the header says what,
// not just how many.
//
// Its own file (extracted from runs.tsx) so that file and its test stay under
// the 1000-line size gate rather than growing past it (scripts/check-file-size.sh).
import * as React from "react";
import { TriangleAlert } from "lucide-react";
import type { AgentRun } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { Chip, RunStateBadge } from "../../wardyn/primitives";
import { RUNS_WAIT } from "../../wardyn/copy";
import { cn } from "../../ui/utils";
import { CardGrid, RunCard } from "./run-card";
import { groupWaitBreakdown, runAttention, type RunSignals } from "./board-groups";

// Per-group collapsed preview before "Show all N".
const GROUP_PREVIEW = 3;

// #160 — the second chip row: one counted chip per wait reason, suppressed
// entirely once every run in the group is terminal (nothing left to wait on),
// and standing in for the whole row with a single "Checking…" chip until the
// approvals fetch resolves — nothing derived from that fetch (held, reauth,
// stale) may paint before then, or it reads as "nothing is held" rather than
// "not known yet". `starting` is a run.state fact, not an approvals one, so
// it renders in the Checking window too.
function WaitRow({
  runs,
  signals,
  signalsResolved,
}: {
  runs: AgentRun[];
  signals: RunSignals;
  signalsResolved: boolean;
}) {
  if (runs.every((r) => isTerminalRunState(r.state))) return null;
  const w = groupWaitBreakdown(runs, signals);
  const chips: React.ReactNode[] = [];
  if (!signalsResolved) {
    chips.push(
      <Chip key="checking" tone="neutral" dot pulse>
        {RUNS_WAIT.CHECKING}
      </Chip>,
    );
  } else {
    if (w.held > 0) {
      chips.push(
        <Chip key="held" tone="warning" dot>
          {RUNS_WAIT.HELD(w.held)}
        </Chip>,
      );
    }
    if (w.reauth > 0) {
      chips.push(
        <Chip key="reauth" tone="warning" dot>
          {RUNS_WAIT.REAUTH(w.reauth)}
        </Chip>,
      );
    }
    if (w.staleHeld > 0) {
      chips.push(
        <Chip key="stale" tone="neutral">
          {RUNS_WAIT.STALE_GROUP(w.staleHeld)}
        </Chip>,
      );
    }
  }
  if (w.starting > 0) {
    chips.push(
      <Chip key="starting" tone="info" dot>
        {RUNS_WAIT.STARTING(w.starting)}
      </Chip>,
    );
  }
  if (chips.length === 0) {
    chips.push(
      <Chip key="none" tone="neutral">
        {RUNS_WAIT.NONE}
      </Chip>,
    );
  }
  return (
    <div className="mt-1.5 flex min-h-[22px] flex-wrap items-center gap-1.5" aria-label="What this group is waiting on">
      {chips}
    </div>
  );
}

export function TitleGroup({
  title,
  runs,
  signals,
  signalsResolved,
  open,
  onToggle,
  onOpen,
  onKill,
}: {
  title: string;
  runs: AgentRun[];
  signals: RunSignals;
  /** Whether the approvals fetch behind `signals` has resolved at least once
   *  — see the "Checking…" note on WaitRow above. */
  signalsResolved: boolean;
  open: boolean;
  onToggle: () => void;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  // Distinct states in the order they appear — which is triage order, since
  // `visible` arrives attention → active → done (see its comment).
  const states: string[] = [];
  for (const r of runs) if (!states.includes(r.state as string)) states.push(r.state as string);
  // Runs that are asking for something are pinned to the lane above, so what
  // is left to flag here is a report — the group carries the danger tint its
  // cards do, not the amber the lane owns. The predicate is the CARD RAIL's,
  // not needsAttention's: that one includes "monitoring" (a passive
  // deny_with_review pending, which no card paints), so a group of two healthy
  // RUNNING runs got a red header over cards with nothing red on them.
  const needsEyes = runs.some((r) => runAttention(r, signals) === "interrupted");
  const shown = open ? runs : runs.slice(0, GROUP_PREVIEW);

  return (
    <section aria-label={title}>
      <div className="mb-1 flex flex-wrap items-center gap-2">
        {needsEyes && <TriangleAlert className="size-3.5 text-danger" aria-hidden="true" />}
        <h2
          className={cn(
            "max-w-[420px] truncate text-body font-semibold",
            needsEyes ? "text-danger" : "text-foreground",
          )}
          title={title}
        >
          {title}
        </h2>
        <span className="rounded-full bg-muted px-1.5 text-meta font-semibold text-muted-foreground">
          {runs.length}
        </span>
        <span className="flex flex-wrap items-center gap-1.5">
          {states.map((st) => {
            const n = runs.filter((r) => (r.state as string) === st).length;
            return (
              <span key={st} className="flex items-center gap-1">
                <RunStateBadge state={st} />
                {n > 1 && <span className="text-meta text-muted-foreground">×{n}</span>}
              </span>
            );
          })}
        </span>
        {/* A disclosure control is a link, not the surface's action —
            CONSOLE-RULES §2 names this exact site: --info, never teal. The
            approvals strip renders the identical string the same way. */}
        {runs.length > GROUP_PREVIEW && (
          <button onClick={onToggle} className="ml-1 text-xs font-medium text-info hover:underline">
            {open ? "Show fewer" : `Show all ${runs.length}`}
          </button>
        )}
      </div>
      <WaitRow runs={runs} signals={signals} signalsResolved={signalsResolved} />
      <CardGrid>
        {shown.map((run) => (
          <RunCard key={run.id} run={run} signals={signals} grouped onOpen={onOpen} onKill={onKill} />
        ))}
      </CardGrid>
    </section>
  );
}
