/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SummaryHeader — run-detail's 52px command bar (design board seg2a's
// `h-[52px]` row, directly above run-detail-command-bar.tsx's `h-9` tabs
// row). Used to be a fat identity card that pushed the live terminal below
// the fold; this is the same identity/state/kill surface compressed into one
// non-wrapping row so the terminal starts at the top of the viewport.
import * as React from "react";
import { Link } from "react-router-dom";
import { Check, Clock, Link as LinkIcon, ShieldAlert, Skull, TerminalSquare } from "lucide-react";
import type { AgentRun } from "../../lib/types";
import { runHeadline } from "../../lib/types";
import { Button } from "../ui/button";
import { AgentBadge, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { RunStateGlyph } from "../wardyn/run-state-glyph";
import { RUN_COCKPIT } from "../wardyn/copy";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { KillRunDialog } from "../wardyn/kill-run-dialog";
import { useOperator, usePrincipal } from "../wardyn/operator-context";


// Exported for the failure block (run-detail/failure-block.tsx), which states
// how far into a run an operator killed it — the same duration, in the same
// words, as the clock in this bar.
export function formatElapsed(ms: number): string {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = totalSeconds % 60;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${s}s`;
}

// Live-ticking duration since `createdAt`, frozen at `updatedAt` once the run
// is terminal — a terminal run's elapsed time must not keep growing every
// time someone happens to reopen the page days later.
function useElapsed(createdAt: string, updatedAt: string, terminal: boolean): string {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    if (terminal) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [terminal]);
  const end = terminal ? new Date(updatedAt).getTime() : now;
  return formatElapsed(end - new Date(createdAt).getTime());
}

export function SummaryHeader({
  run,
  terminal,
  exitCode,
  pendingApprovalCount = 0,
  sandboxHeld = false,
  onCopyLink,
  linkCopied = false,
  onKill,
}: {
  run: AgentRun;
  terminal: boolean;
  // The agent's own exit code (audit-derived); undefined when none was recorded.
  exitCode?: number;
  // Count of this run's PENDING approvals (run-detail.tsx already computes
  // this for the Approvals tab badge — same number, surfaced here too so it's
  // visible without switching tabs).
  pendingApprovalCount?: number;
  // At least one of those pending approvals is HOLDING the sandbox right now
  // (a wait_for_review first-use gate — the connection is parked until someone
  // decides). Computed by the parent with live-approvals.tsx's own exported
  // isHeld, so this chip and the strip under the terminal can never disagree.
  sandboxHeld?: boolean;
  // Copy this run's permalink. The old screen had a Copy-link button in a
  // breadcrumb row that the command bar replaced; the affordance survives the
  // row it lived in.
  onCopyLink?: () => void;
  linkCopied?: boolean;
  onKill: () => void;
}) {
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  // W25-1: claim "attachable" only under the SAME owner-or-admin predicate
  // AttachTerminal itself gates the connect on (attach-terminal.tsx: `!operator
  // && !owned`) — otherwise a member sees this chip promise attachability and
  // then gets a red "requires the admin role" error the instant they open
  // the terminal below it (OverviewTab renders <AttachTerminal> whenever
  // `attachable`).
  const operator = useOperator();
  const principal = usePrincipal();
  const owned = !!run.created_by && run.created_by === principal;
  const canAttach = operator || owned;
  const elapsed = useElapsed(run.created_at, run.updated_at, terminal);
  const shortId = run.id.replace(/^run_/, "");

  return (
    <div className="flex h-[52px] min-w-0 shrink-0 items-center gap-3 border-b border-border bg-card px-4">
      <Link to="/runs" className="shrink-0 text-sm text-muted-foreground hover:text-foreground">
        Runs
      </Link>
      <span className="h-5 w-px shrink-0 bg-border" aria-hidden="true" />

      {/* M3: WHO beside WHAT, one adjacent pair (CONSOLE-RULES §5) — never
          fused, and never separated across the bar. The identity glyph used to
          sit here alone while the state lived four elements away past the repo
          and the workspace path, so the same run read as two different shapes
          on the board and in the cockpit. Same component the board's card
          uses, off the same signals, so board -> cockpit has no seam. */}
      <span className="flex shrink-0 items-center gap-1.5">
        <AgentBadge agent={run.agent} withLabel={false} />
        <RunStateGlyph
          state={run.state}
          signals={{
            // The page already fetched both facts for the pending chip below:
            // a HELD approval outranks a merely pending one, which is exactly
            // attentionRank's own order (permission > monitoring).
            held: sandboxHeld,
            passiveHold: pendingApprovalCount > 0,
          }}
        />
      </span>

      {/* Requirement: run.task stays an h1 (the board drops it, but for an
          autonomous run this is the only statement anywhere on the page of
          what it's doing — and the page's only h1). */}
      <h1
        className="min-w-0 flex-1 truncate text-sm font-semibold text-foreground"
        title={run.task || undefined}
      >
        {runHeadline(run)}
      </h1>

      {/* repo / workspace path — required to stay visible (not "genuinely
          secondary"), so no `hidden lg:` gate; each truncates on its own. */}
      <div className="flex min-w-0 shrink items-baseline gap-2">
        <span className="max-w-[180px] truncate font-mono text-xs text-foreground" title={run.repo}>
          {run.repo}
        </span>
        {run.workspace_path && (
          <span
            className="max-w-[220px] truncate font-mono text-xs text-muted-foreground"
            title={run.workspace_path}
          >
            {run.workspace_path}
          </span>
        )}
        {/* short run id — genuinely secondary (not in the "must carry" list). */}
        <span className="hidden shrink-0 font-mono text-xs text-muted-foreground lg:inline" title={run.id}>
          · {shortId}
        </span>
        {onCopyLink && (
          <button
            type="button"
            onClick={onCopyLink}
            title="Copy link to this run"
            aria-label="Copy link to this run"
            className="hidden shrink-0 rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground lg:inline-flex"
          >
            {linkCopied ? <Check className="size-3 text-success" /> : <LinkIcon className="size-3" />}
          </button>
        )}
      </div>

      <RunStateBadge state={run.state} />

      {/* A FAILED run said nothing about WHY anywhere in the console — the
          agent's exit code was CLI-only (wardyn run --wait). Stays visible at
          every width: it's the only place a FAILED run says why. */}
      {exitCode !== undefined && (
        <Chip tone={exitCode === 0 ? "neutral" : "danger"} mono className="shrink-0">
          exit {exitCode}
        </Chip>
      )}

      {/* D9: the pre-agent-start failure class (mount failure, etc.) never
          gets an exit code at all — this is the only place THAT run says
          why. Independent of the exit chip above: a run can show one, the
          other, both, or neither. Bare server text, no prefix — the state
          badge already says "Failed". */}
      {run.failure_hint && (
        <Chip tone="danger" className="max-w-[280px] shrink-0 truncate" title={run.failure_hint}>
          {run.failure_hint}
        </Chip>
      )}

      {/* Confinement/barrier + interactive-attach detail — genuinely
          secondary next to state/repo/workspace/elapsed/pending/Kill. */}
      <div className="hidden shrink-0 items-center gap-2 lg:flex">
        <ConfinementChip value={run.confinement_class} />
        <BarrierStrengthStrip tier={run.confinement_class} />
        {run.interactive && (
          <Chip tone="info" className="gap-1">
            <TerminalSquare className="size-3" />
            {run.state === "RUNNING" && canAttach ? "Interactive — attachable" : "Interactive"}
          </Chip>
        )}
      </div>

      {/* A bare "4m 22s" beside an unlabelled clock does not say WHAT it
          measures. The title/aria-label do, without spending command-bar
          width on the word "elapsed". */}
      <span
        className="flex shrink-0 items-center gap-1.5 font-mono text-xs text-muted-foreground"
        title={terminal ? "Total run time" : "Time since this run started"}
        aria-label={`${terminal ? "Total run time" : "Running for"} ${elapsed}`}
      >
        <Clock className="size-3" aria-hidden="true" />
        {elapsed}
      </span>

      <div className="ml-auto flex shrink-0 items-center gap-2">
        {pendingApprovalCount > 0 && (
          <Chip tone="warning" className="h-7 gap-1">
            <ShieldAlert className="size-3" />
            {sandboxHeld
              ? RUN_COCKPIT.waitingHeld(pendingApprovalCount)
              : RUN_COCKPIT.waiting(pendingApprovalCount)}
          </Chip>
        )}
        <Button
          variant="outline"
          size="sm"
          className="h-7 text-danger hover:text-danger"
          disabled={terminal}
          onClick={() => setConfirmId(run.id)}
        >
          <Skull className="size-4" /> Kill
        </Button>
        <KillRunDialog
          runId={confirmId}
          onOpenChange={(o) => !o && setConfirmId(null)}
          onConfirm={onKill}
        />
      </div>
    </div>
  );
}
