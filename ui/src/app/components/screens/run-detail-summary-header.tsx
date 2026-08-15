/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SummaryHeader — the run-detail screen's top identity card (agent, task,
// state chips, kill button). Split from run-detail.tsx along the design's
// own seam when the file crossed the size gate; run-detail-ssh.tsx already
// got the same split for the SSH-connect card.
import * as React from "react";
import { Skull, TerminalSquare } from "lucide-react";
import type { AgentRun } from "../../lib/types";
import { relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { AgentBadge, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { KillRunDialog } from "../wardyn/kill-run-dialog";
import { useOperator, usePrincipal } from "../wardyn/operator-context";

export function SummaryHeader({
  run,
  terminal,
  exitCode,
  onKill,
}: {
  run: AgentRun;
  terminal: boolean;
  // The agent's own exit code (audit-derived); undefined when none was recorded.
  exitCode?: number;
  onKill: () => void;
}) {
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  // W25-1: claim "attachable" only under the SAME owner-or-admin predicate
  // AttachTerminal itself gates the connect on (attach-terminal.tsx: `!operator
  // && !owned`) — otherwise a member sees this chip promise attachability and
  // then gets a red "requires the operator role" error the instant they open
  // the terminal below it (OverviewTab renders <AttachTerminal> whenever
  // `attachable`).
  const operator = useOperator();
  const principal = usePrincipal();
  const owned = !!run.created_by && run.created_by === principal;
  const canAttach = operator || owned;
  return (
    <div className="rounded-xl border border-border bg-card p-5">
      <div className="flex flex-wrap items-start gap-4">
        <AgentBadge agent={run.agent} withLabel={false} />
        <div className="min-w-[260px] flex-1">
          <h1 className="text-xl font-semibold leading-tight text-foreground">{run.task || "—"}</h1>
          <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
            <AgentBadge agent={run.agent} />
            <span>·</span>
            <span className="font-mono">{run.repo}</span>
            <span>·</span>
            <span title={run.created_at}>started {relativeTime(run.created_at)} by {run.created_by}</span>
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <RunStateBadge state={run.state} />
            {/* A FAILED run said nothing about WHY anywhere in the console —
                the agent's exit code was CLI-only (wardyn run --wait). */}
            {exitCode !== undefined && (
              <Chip tone={exitCode === 0 ? "neutral" : "danger"} mono>
                agent exit {exitCode}
              </Chip>
            )}
            <ConfinementChip value={run.confinement_class} />
            <BarrierStrengthStrip tier={run.confinement_class} />
            {run.interactive && (
              <Chip tone="info" className="gap-1">
                <TerminalSquare className="size-3" />
                {run.state === "RUNNING" && canAttach ? "Interactive — attachable" : "Interactive"}
              </Chip>
            )}
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="text-danger hover:text-danger"
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
