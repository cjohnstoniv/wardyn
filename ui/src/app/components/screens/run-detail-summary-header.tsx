/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SummaryHeader — run-detail's header, directly above run-detail-command-bar.tsx's
// tabs row. Two rows: the title with its actions, then the run's state once
// and the facts about it as plain, visible text (nothing hides in a tooltip).
import * as React from "react";
import { Link } from "react-router-dom";
import { ChevronDown, Laptop, MoreHorizontal, Pencil, RotateCcw, ShieldAlert, Skull, TerminalSquare } from "lucide-react";
import type { RunDetail } from "../../lib/types";
import { runHeadline } from "../../lib/types";
import { formatDuration, shortTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { AutonomyChip, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { RUN, RUN_COCKPIT } from "../wardyn/copy";
import { waitingAdoConsent, waitingReauth } from "../../lib/reauth-waiting-copy";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { RUN_HEADER } from "../wardyn/copy/run-header";
import { KillRunDialog } from "../wardyn/kill-run-dialog";
import { useOperator, useOperatorResolved, usePrincipal } from "../wardyn/operator-context";
import { mayEnterRunOrUnknown } from "../../lib/run-entry";
import { RUN_FACTS } from "../wardyn/copy/door";
import { RUN_PLACEMENT, runnerLabel } from "../wardyn/copy/runners";
import { PENDING_NO_DETAIL, statusDetailSentence } from "./run-status-detail";
import { titleFromTask } from "../../lib/run-title";


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
  return formatDuration(end - new Date(createdAt).getTime());
}

// The model provider this run chose at create (#543, decision 5) — fixed on the
// run, so it never changes after launch. The server names it and says when it
// is gone (#996); the id stands in only when it has no name, and "(removed)" is
// said only when the server says deleted.
function RunProviderChip({ run }: { run: RunDetail }) {
  const name = run.model_provider_name || run.model_provider_id || "";
  return <Chip tone="neutral">{RUN_FACTS.PROVIDER(name, !!run.model_provider_deleted)}</Chip>;
}

// What the run is doing, as one sentence. A starting run says what it waits on; a run
// with a decision pending says it is waiting on you; every other state says its own fact.
function statusSentence(run: RunDetail, terminal: boolean, pendingApprovalCount: number, viewerBlocked: boolean): string {
  if (run.state === "PENDING" && !run.status_detail?.trim()) return PENDING_NO_DETAIL;
  if (run.state === "STARTING" || run.state === "PENDING") {
    return statusDetailSentence(run.status_detail, run.status_reason) || RUN_HEADER.STARTING;
  }
  if (!terminal && pendingApprovalCount > 0) return viewerBlocked ? RUN_HEADER.WAITING_FOR_ADMIN : RUN_HEADER.WAITING;
  switch (run.state) {
    case "RUNNING":
      return RUN_HEADER.WORKING;
    case "COMPLETED":
      return RUN_HEADER.FINISHED;
    case "STOPPED":
      return RUN_HEADER.STOPPED;
    case "FAILED":
      return RUN_HEADER.FAILED;
    case "KILLED":
      return RUN_HEADER.KILLED;
    case "ARCHIVED":
      return RUN_HEADER.ARCHIVED;
    case "QUEUED":
      return RUN_HEADER.QUEUED;
    default:
      return "";
  }
}

export function SummaryHeader({
  run,
  terminal,
  exitCode,
  pendingApprovalCount = 0,
  viewerBlocked = false,
  sandboxHeld = false,
  awaitingReauth = false,
  awaitingAdoConsent = false,
  onGoApprovals,
  onCopyLink,
  onKill,
  canKillAgain,
  onClone,
  onRename,
}: {
  run: RunDetail;
  terminal: boolean;
  // The agent's own exit code (audit-derived); undefined when none was recorded.
  exitCode?: number;
  // Count of this run's PENDING approvals (run-detail.tsx already computes
  // this for the Approvals tab badge — same number, surfaced here too so it's
  // visible without switching tabs).
  pendingApprovalCount?: number;
  /** The pending decision is one this viewer cannot make (viewerBlockedOnDecision). */
  viewerBlocked?: boolean;
  // At least one of those pending approvals is HOLDING the sandbox right now
  // (a wait_for_review first-use gate — the connection is parked until someone
  // decides). Computed by the parent with live-approvals.tsx's own exported
  // isHeld, so this chip and the strip under the terminal can never disagree.
  sandboxHeld?: boolean;
  // At least one of those pending approvals is a mid-run AWS SIGN-IN request.
  // It gets its own wording because "sandbox held" sends the person looking for
  // an Approve button that does not exist for this kind — and because this is the
  // one hold they can clear themselves. The count rides the sentence, so a
  // co-pending egress approval is not hidden behind the sign-in.
  awaitingReauth?: boolean;
  // The Azure DevOps twin of awaitingReauth (S10 round 2, F13) — the SAME
  // shape (a mid-run sign-in hold this run's owner can clear themselves) for
  // a DIFFERENT provider. Kept as its own prop, never folded into
  // awaitingReauth: the chip below must never say "AWS" for this one.
  awaitingAdoConsent?: boolean;
  // Opens the Approvals tab: the "N waiting" chip becomes a link to it.
  onGoApprovals?: () => void;
  // Copy this run's permalink, from the More menu.
  onCopyLink?: () => void;
  onKill: (runId: string) => void;
  // A KILLED run whose trail does not prove the teardown (#1487): Kill stays
  // so the person can run the cascade again.
  canKillAgain?: boolean;
  // "Start a run like this one", for EVERY terminal run — the failure block
  // only renders it for a run that ended badly (3 of the 5 terminal states),
  // so it belongs here instead. Optional so the header stays renderable
  // without a parent that owns navigation (the unit tests below).
  onClone?: () => void;
  // Rename (#1197 L2): owner only (this component's own `owned` check below),
  // absent for anyone else — same optional-prop shape as onClone, so the
  // header stays renderable with no parent that owns the write.
  onRename?: (title: string) => void;
}) {
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  // Rename's edit mode: the form renders as its own full-width row under the header.
  const [renaming, setRenaming] = React.useState(false);
  const [draftTitle, setDraftTitle] = React.useState("");
  // Claim "attachable" only under the SAME entry rule AttachTerminal itself
  // gates the connect on (lib/run-entry.ts) — otherwise a viewer sees this chip
  // promise attachability and then gets a refusal the instant they open the
  // terminal below it (OverviewTab renders <AttachTerminal> whenever
  // `attachable`). #1476: the run's person, or an admin on a run no person owns.
  const operator = useOperator();
  const principal = usePrincipal();
  const owned = !!run.created_by && run.created_by === principal;
  const canAttach = mayEnterRunOrUnknown(run, principal, operator, useOperatorResolved());
  const elapsed = useElapsed(run.created_at, run.updated_at, terminal);
  const sentence = statusSentence(run, terminal, pendingApprovalCount, viewerBlocked);
  const startRename = () => {
    // F3 (review round 2): never seed from the WHOLE task —
    // runHeadline's own fallback chain (title, then task, then
    // "—") means an untitled run with a long or multi-line task
    // seeded a draft the server would refuse on Save (a title
    // caps at 200 chars and refuses a newline), and a task-less
    // run seeded the literal "—". Same first-line/80-char/
    // word-boundary default New Run itself prefills from, or the
    // run's own trimmed title when it has one.
    setDraftTitle(run.title?.trim() || titleFromTask(run.task ?? ""));
    setRenaming(true);
  };
  const canRename = !!onRename && owned && !renaming;
  const pendingText = awaitingAdoConsent
    ? // Azure DevOps, never AWS (F13) — checked ahead of awaitingReauth, same ownership rule.
      waitingAdoConsent(pendingApprovalCount, owned)
    : awaitingReauth
      ? /* `owned` — the chip says whose sign-in is awaited, and only the run's owner can give it (W6-U SHOULD-1). */
        waitingReauth(pendingApprovalCount, owned)
      : sandboxHeld
        ? RUN_COCKPIT.waitingHeld(pendingApprovalCount)
        : RUN_COCKPIT.waiting(pendingApprovalCount);
  const pendingChip = (
    <Chip tone="warning" className="h-7 gap-1">
      <ShieldAlert className="size-3 shrink-0" />
      <span data-testid="pending-approval-chip-text" className="block min-w-0">
        {onGoApprovals && !viewerBlocked ? `${pendingText} · ${RUN_HEADER.REVIEW}` : pendingText}
      </span>
    </Chip>
  );

  return (
    <>
    {/* A stable e2e hook, scoping "Interactive"/"Fence" text assertions to this header rather than the whole page. */}
    <div
      data-testid="run-summary-header"
      className="flex min-w-0 shrink-0 flex-col gap-2 border-b border-border bg-card px-4 py-2.5"
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
        <div className="flex basis-full items-center gap-2 sm:basis-auto">
          <Link to="/runs" className="shrink-0 text-sm text-muted-foreground hover:text-foreground">
            Runs
          </Link>
          <span className="shrink-0 text-muted-foreground" aria-hidden="true">/</span>
        </div>
        {/* The page's only h1: for an autonomous run it is the one statement of what the run is doing. */}
        <h1 className="min-w-[8rem] flex-1 truncate text-lg font-semibold text-foreground sm:text-base" title={run.task || undefined}>
          {runHeadline(run)}
        </h1>
        {canRename && (
          <Button variant="ghost" size="sm" className="hidden shrink-0 sm:inline-flex" onClick={startRename}>
            <Pencil className="size-4" /> {RUN_COCKPIT.rename}
          </Button>
        )}
        {/* The primary action leads: Start-a-run-like-this on a run that ended, Kill on one that has not.
            Neither is the header's danger colour except Kill itself. */}
        {terminal && onClone && (
          <Button variant="outline" size="sm" className="shrink-0" onClick={onClone}>
            <RotateCcw className="size-4" /> {RUN.CLONE_CTA}
          </Button>
        )}
        {(!terminal || canKillAgain) && (
          <Button
            variant="outline"
            size="sm"
            className="shrink-0 text-danger hover:text-danger"
            onClick={() => setConfirmId(run.id)}
          >
            <Skull className="size-4" /> Kill
          </Button>
        )}
        {(canRename || onCopyLink) && (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="sm" className="shrink-0" aria-label={RUN_HEADER.MORE}>
                <span className="hidden sm:inline">{RUN_HEADER.MORE}</span>
                <ChevronDown className="hidden size-4 sm:inline" aria-hidden="true" />
                <MoreHorizontal className="size-5 sm:hidden" aria-hidden="true" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {canRename && (
                <DropdownMenuItem className="sm:hidden" onSelect={startRename}>
                  {RUN_COCKPIT.rename}
                </DropdownMenuItem>
              )}
              {onCopyLink && <DropdownMenuItem onSelect={onCopyLink}>Copy link to this run</DropdownMenuItem>}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        <KillRunDialog
          runId={confirmId}
          title={run.title?.trim() || run.task}
          onOpenChange={(o) => !o && setConfirmId(null)}
          onConfirm={onKill}
        />
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-sm">
        <RunStateBadge state={run.state} />
        {sentence && (
          <span data-testid="run-status-sentence" className="min-w-0 text-foreground [overflow-wrap:anywhere]">
            {sentence}
          </span>
        )}
        {pendingApprovalCount > 0 &&
          (onGoApprovals ? (
            <button
              type="button"
              onClick={onGoApprovals}
              className="rounded-md focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
            >
              {pendingChip}
            </button>
          ) : (
            pendingChip
          ))}
        <span className="font-mono text-xs text-foreground" title={run.repo}>
          {run.repo}
        </span>
        {run.workspace_path && (
          <span className="min-w-0 max-w-[220px] truncate font-mono text-xs text-muted-foreground" title={run.workspace_path}>
            {run.workspace_path}
          </span>
        )}
        {/* A FAILED run said nothing about WHY anywhere in the console — the agent's exit code was CLI-only
            (wardyn run --wait). */}
        {exitCode !== undefined && (
          <Chip tone={exitCode === 0 ? "neutral" : "danger"} mono>
            exit {exitCode}
          </Chip>
        )}
        {/* D9: the pre-agent-start failure class (mount failure, etc.) never gets an exit code at all.
            Independent of the exit chip: a run can show one, the other, both, or neither. Bare server text,
            no prefix — the state badge already says "Failed". */}
        {run.failure_hint && (
          <Chip tone="danger" className="min-w-0 max-w-full" title={run.failure_hint}>
            <span className="block min-w-0 truncate">{run.failure_hint}</span>
          </Chip>
        )}
        {run.model_provider_id && <RunProviderChip run={run} />}
        <ConfinementChip value={run.confinement_class} />
        {/* 0.9 (M-RN-3 RS3): where a LOCAL run is. Neutral, and absent for a remote run, whose default
            place needs no chip (RN-Q14), so an ordinary run's header is what it was. */}
        {run.placement === "local" && (
          <Chip tone="neutral" className="gap-1">
            <Laptop className="size-3" aria-hidden />
            {RUN_PLACEMENT.CHIP(runnerLabel(run))}
          </Chip>
        )}
        {/* run.autonomy_level (#97) freezes the level resolveRunAutonomy capped this run at, at create time.
            Empty for a run under no profile, a profile with no rubric, or one created before this field
            existed. */}
        {run.autonomy_level && <AutonomyChip level={run.autonomy_level} />}
        {run.interactive && (
          <Chip tone="info" className="gap-1">
            <TerminalSquare className="size-3" />
            {run.state === "RUNNING" && canAttach ? "Interactive — attachable" : "Interactive"}
          </Chip>
        )}
        <span className="text-muted-foreground" aria-label={`${terminal ? "Total run time" : "Running for"} ${elapsed}`}>
          {terminal ? RUN_HEADER.RAN(elapsed, shortTime(run.updated_at)) : elapsed}
        </span>
      </div>
    </div>
    {/* #1234: how a portal-launched run got here, one thin muted line directly
        under the header. Not a chip: a sentence reads better than a chip for a
        provenance fact. Absent for a self-launched run, so the page is what it
        was. The server names the portal (a revoked one keeps its name); when it
        cannot, the line still says a portal did it. */}
    {run.created_via && (
      <p data-testid="run-launched-via" className="break-words border-b border-border bg-card px-4 py-1 text-xs text-muted-foreground">
        {run.created_via_name ? RUN_FACTS.LAUNCHED_VIA(run.created_via_name) : RUN_FACTS.LAUNCHED_VIA_FALLBACK}
      </p>
    )}
    {/* Rename's edit row (#1197 L2): a FULL-WIDTH row under the header, matching the mock's own layout
        (packet:742, the heading's own slot replaced by a wrapping, width:100% form). */}
    {renaming && onRename && (
      <form
        className="flex flex-wrap items-center gap-2 border-b border-border bg-card px-4 py-2"
        onSubmit={(e) => {
          e.preventDefault();
          // F5 (review round 2): an emptied draft is Cancel, not a "renamed
          // to blank" success — the mock's own handler only submits `if
          // (v)` (packet.js), and a false RENAMED toast for a clear the
          // operator likely did not intend is worse than a quiet close.
          if (draftTitle.trim()) onRename(draftTitle.trim());
          setRenaming(false);
        }}
      >
        <label htmlFor="run-rename-title" className="text-xs font-medium text-muted-foreground">
          Title
        </label>
        <Input
          id="run-rename-title"
          autoFocus
          maxLength={200}
          className="h-8 min-w-[240px] flex-1"
          value={draftTitle}
          onChange={(e) => setDraftTitle(e.target.value)}
        />
        <Button type="submit" size="sm">
          Save
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => setRenaming(false)}>
          Cancel
        </Button>
      </form>
    )}
    </>
  );
}
