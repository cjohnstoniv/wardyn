/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SummaryHeader — run-detail's command bar (design board seg2a's `h-[52px]`
// row, directly above run-detail-command-bar.tsx's `h-9` tabs row). A fat
// identity card pushes the live terminal below the fold, so this
// identity/state/kill surface is compressed into one 52px row — but only at
// `xl` (1280px) and up; it wraps onto a second line below that, since not
// every fact this bar carries fits in one row's width down at `lg` (1024px).
import * as React from "react";
import { Link } from "react-router-dom";
import { Check, Clock, Link as LinkIcon, RotateCcw, ShieldAlert, Skull, TerminalSquare } from "lucide-react";
import type { AgentRun } from "../../lib/types";
import { runHeadline } from "../../lib/types";
import { Button } from "../ui/button";
import { AgentBadge, AutonomyChip, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { RunStateGlyph } from "../wardyn/run-state-glyph";
import { RUN, RUN_COCKPIT } from "../wardyn/copy";
import { waitingAdoConsent, waitingReauth } from "../../lib/reauth-waiting-copy";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { KillRunDialog } from "../wardyn/kill-run-dialog";
import { useOperator, usePrincipal } from "../wardyn/operator-context";
import {
  CHIP_QUEUED,
  isTerminalStatusReason,
  PENDING_NO_DETAIL,
  statusDetailChip,
  statusDetailSentence,
} from "./run-status-detail";


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
  awaitingReauth = false,
  awaitingAdoConsent = false,
  onCopyLink,
  linkCopied = false,
  onKill,
  onClone,
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
  // At least one of those pending approvals is a mid-run AWS SIGN-IN request.
  // It gets its own chip because "sandbox held" sends the person looking for an
  // Approve button that does not exist for this kind — and because this is the
  // one hold they can clear themselves. The count rides the sentence, so a
  // co-pending egress approval is not hidden behind the sign-in.
  awaitingReauth?: boolean;
  // The Azure DevOps twin of awaitingReauth (S10 round 2, F13) — the SAME
  // shape (a mid-run sign-in hold this run's owner can clear themselves) for
  // a DIFFERENT provider. Kept as its own prop, never folded into
  // awaitingReauth: the chip below must never say "AWS" for this one.
  awaitingAdoConsent?: boolean;
  // Copy this run's permalink. The old screen had a Copy-link button in a
  // breadcrumb row that the command bar replaced; the affordance survives the
  // row it lived in.
  onCopyLink?: () => void;
  linkCopied?: boolean;
  onKill: () => void;
  // "Start a run like this one", for EVERY terminal run — the failure block
  // only renders it for a run that ended badly (3 of the 5 terminal states),
  // so it belongs here instead. Optional so the header stays renderable
  // without a parent that owns navigation (the unit tests below).
  onClone?: () => void;
}) {
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  // Claim "attachable" only under the SAME owner-or-admin predicate
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
  // "" whenever there is nothing to say. The SERVER has already blanked
  // status_detail for every run that is not STARTING (PENDING included —
  // projectStatusDetail's `default` branch in runs_status_detail.go blanks it
  // for PENDING exactly like every other non-STARTING state) except a FAILED
  // one whose reason IS the failure — and the header gates on STARTING/PENDING
  // anyway, because a FAILED run's header already says why in the failure_hint
  // chip below, and two chips narrating one ending is how a bar this crowded
  // loses the one that matters.
  //
  // #125: PENDING's own first tick carries no status_detail at all — the
  // ordinary derivation below would render nothing — so that ONE case reaches
  // for PENDING_NO_DETAIL instead. The moment status_detail carries a real
  // stage line (today, only ever once the run has moved to STARTING) this
  // falls through to the ordinary STARTING/PENDING derivation, which
  // supersedes it.
  const pendingQueued = run.state === "PENDING" && !run.status_detail?.trim();
  // SF-25: the chip below is the SHORT register, same as every STARTING/PENDING
  // reason (statusDetailChip) — PENDING_NO_DETAIL is the header chip's `title`
  // tooltip's sentence, never the chip's own truncatable text (max-w-[160px]
  // truncates the 42-character sentence to a fragment, the same failure this
  // chip's own doc comment already warns statusDetailChip exists to avoid).
  // The sentence also rides a visible line below the bar (the `pendingQueued`
  // block near the end of this component) — the title tooltip alone cannot
  // be read on a touch device.
  const statusChip = pendingQueued
    ? CHIP_QUEUED
    : run.state === "STARTING" || run.state === "PENDING"
      ? statusDetailChip(run.status_detail, run.status_reason)
      : "";

  // This is a stable e2e hook, scoping "Interactive"/"Fence" text
  // assertions to this bar rather than the whole page (both strings are
  // plain text, so a future widget rendering either one elsewhere would
  // otherwise turn a passing check into a Playwright strict-mode failure).
  //
  // The bar is a single non-wrapping row ONLY at xl (1280) and up — below
  // that (lg, 1024, this suite's other tested width) it wraps onto a second
  // line instead. This is the fix, not a fallback: at 1024 every "must never
  // yield" fact (h1 >= 160, repo >= 90, ConfinementChip + Interactive, the
  // clone button's full label, Kill on-screen) together measure wider than
  // the row has, and NOTHING left in the "may yield" waterfall
  // (failure_hint/copy-link/short-id/BarrierStrengthStrip/elapsed, all
  // already hidden below 2xl) is enough to close that gap without cutting
  // something this file treats as non-negotiable. Wrapping costs zero
  // information and touches no floor — line 1 carries identity/task/repo/
  // state, line 2 carries barrier/action — at the cost of a taller bar
  // (min-h, not h, below xl) only on widths narrower than this console's
  // primary target.
  return (
    <>
    <div
      data-testid="run-summary-header"
      // overflow-hidden — the single-line xl+ row still floors near
      // 1300px with every "must never yield" element at its floor; the two
      // shrinkable chips above degrade width, this catches whatever's left
      // rather than letting <main>'s overflow-y:auto force overflow-x too.
      className="flex h-auto min-h-[52px] min-w-0 shrink-0 flex-wrap items-center gap-3 overflow-hidden border-b border-border bg-card px-4 py-1.5 xl:h-[52px] xl:flex-nowrap xl:py-0"
    >
      <Link to="/runs" className="shrink-0 text-sm text-muted-foreground hover:text-foreground">
        Runs
      </Link>
      <span className="h-5 w-px shrink-0 bg-border" aria-hidden="true" />

      {/* WHO beside WHAT, one adjacent pair (CONSOLE-RULES §5) — never
          fused, and never separated across the bar: the identity glyph and
          the state must sit together, or the same run reads as two different
          shapes on the board and in the cockpit. Same component the board's
          card uses, off the same signals, so board -> cockpit has no seam. */}
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
          what it's doing — and the page's only h1). `flex-1` alone is
          `flex: 1 1 0%` — a ZERO base size, so under negative free space this
          element gets none of the shrink and a sibling with real content (the
          repo/workspace group) absorbs the whole deficit instead.
          min-w-[160px] makes the floor the width trade is measured against
          (runs.spec.ts) an enforced CSS invariant, not a fixture that happens
          not to reach the squeeze. */}
      <h1
        className="min-w-[160px] flex-1 truncate text-sm font-semibold text-foreground"
        title={run.task || undefined}
      >
        {runHeadline(run)}
      </h1>

      {/* repo — required to stay visible (not "genuinely secondary"), so it
          never hides at any width; only its cap narrows (180 -> 140 below
          2xl). min-w-[90px] on BOTH this span and its wrapping div (≈11 mono
          chars, an org segment) is an enforced floor, not a last-resort
          guess — a min-width on the SPAN alone lets it overflow its own flex
          parent once that parent has been shrunk to 0 by the row (a
          live-browser measurement caught this: it rendered repo overlapping
          the "Failed"/exit chips rather than pushing them over). Putting the
          same floor on the wrapping div is what makes the row actually
          RESERVE the space instead of just letting repo bleed into it. */}
      <div className="flex min-w-[90px] shrink items-baseline gap-2">
        <span className="min-w-[90px] max-w-[140px] truncate font-mono text-xs text-foreground 2xl:max-w-[180px]" title={run.repo}>
          {run.repo}
        </span>
        {/* Hidden below 2xl, joining the short-id span right after it in the
            same waterfall step — an UNBOUNDED mount path is the single
            biggest thing this bar can be asked to carry, unlike repo (this
            file's own long-standing "required to stay visible" fact). Hiding
            it, not just capping it, is what actually lets repo keep a real
            min-width floor instead of being crushed to 0 alongside it in the
            same shrinkable group. */}
        {run.workspace_path && (
          <span
            className="hidden min-w-0 max-w-[220px] truncate font-mono text-xs text-muted-foreground 2xl:inline"
            title={run.workspace_path}
          >
            {run.workspace_path}
          </span>
        )}
        {/* short run id — genuinely secondary (not in the "must carry" list),
            hidden below 2xl. A full run id is unbounded width, unlike repo's
            own capped-and-floored span — showing both below 2xl would squeeze
            the task h1 to zero without the h1's own floor and this bar's
            ability to wrap. */}
        <span className="hidden shrink-0 font-mono text-xs text-muted-foreground 2xl:inline" title={run.id}>
          · {shortId}
        </span>
        {/* Bumped lg -> 2xl, same waterfall step as the short id right above
            it — this button's ~20px was part of what the failure_hint chip's
            readability was costing. */}
        {onCopyLink && (
          <button
            type="button"
            onClick={onCopyLink}
            title="Copy link to this run"
            aria-label="Copy link to this run"
            className="hidden shrink-0 rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground 2xl:inline-flex"
          >
            {linkCopied ? <Check className="size-3 text-success" /> : <LinkIcon className="size-3" />}
          </button>
        )}
      </div>

      <RunStateBadge state={run.state} />

      {/* What a STARTING run is waiting ON. The SHORT register
          (statusDetailChip), never the sentence — this chip is max-w-[160px]
          like failure_hint below, so the full sentence truncates to a
          restatement of the badge right beside it ("Starting the sandbo…") and
          the registry's own words, the entire point of a terminal reason, never
          reach the screen. The sentence rides the `title`, where width is free.
          Tone is `warning`, not `info`, when the reason is TERMINAL: an info
          chip on a start that is already over reads as progress.
          Same min-w-0 shrink truncate + "may never hide at any width" treatment
          as failure_hint — the server has already blanked status_detail for
          every run this must not speak for. */}
      {statusChip && (
        <Chip
          tone={isTerminalStatusReason(run.status_reason) ? "warning" : "info"}
          className="min-w-0 max-w-[160px] shrink"
          title={pendingQueued ? PENDING_NO_DETAIL : statusDetailSentence(run.status_detail, run.status_reason)}
        >
          <span className="block min-w-0 truncate">{statusChip}</span>
        </Chip>
      )}

      {/* A FAILED run said nothing about WHY anywhere in the console — the
          agent's exit code was CLI-only (wardyn run --wait). Stays visible at
          every width: it's the only place a FAILED run says why. */}
      {exitCode !== undefined && (
        // min-w-0 shrink truncate, not shrink-0 — this chip is short
        // ("exit N") but must degrade like failure_hint below rather than be
        // the thing that forces the bar wider than the viewport.
        <Chip tone={exitCode === 0 ? "neutral" : "danger"} mono className="min-w-0 shrink truncate">
          exit {exitCode}
        </Chip>
      )}

      {/* D9: the pre-agent-start failure class (mount failure, etc.) never
          gets an exit code at all. Independent of the exit chip above: a run
          can show one, the other, both, or neither. Bare server text, no
          prefix — the state badge already says "Failed".
          This chip is NOT the only place THAT run says why — run-detail/
          failure-block.tsx's What-happened body also renders the full
          sentence for the `unknown` ending kind (D9's own class), where
          width is free.
          `:164-172` above documents this chip as "the only place a FAILED
          run says why — stays visible at every width", so it must NEVER
          hide, at 2xl or any width below it — a `hidden … 2xl:inline-flex`
          treatment here is exactly that regression. min-w-0 shrink lets it
          give up WIDTH instead of existence; the overflow guard is the bar's
          own overflow-hidden below, not this chip disappearing.
          `truncate` on an inline-flex Chip clips mid-word with NO ellipsis
          (min-content sizing on the anonymous flex child) — the inner span
          below is a real block box, so text-overflow actually paints one. */}
      {run.failure_hint && (
        <Chip tone="danger" className="min-w-0 max-w-[160px] shrink" title={run.failure_hint}>
          <span className="block min-w-0 truncate">{run.failure_hint}</span>
        </Chip>
      )}

      {/* Confinement + interactive-attach: NOT secondary —
          (governance.spec.ts:473, security admin reaching a run they don't
          own) reads "Interactive" vs "Interactive — attachable" as the
          security-visible fact that a caller cannot open a PTY on someone
          else's run, and the run's own ConfinementChip is the ONLY place a
          run's tier shows at all — the header's global chip is gone, so
          there is no width gate here either. A `hidden … lg:flex` treatment
          here would be survivable only while an app-shell global BarrierChip
          still covered <1024; with that gone, every width below 1024 must
          show tier and attachability somewhere on the run page. The bar
          wraps, so showing these costs a line, not information — and
          runs.spec.ts's width loop measures 800px alongside 1024/1280/1536. */}
      <div className="flex shrink-0 items-center gap-2">
        <ConfinementChip value={run.confinement_class} />
        {/* run.autonomy_level (#97) freezes the level resolveRunAutonomy
            capped this run at, at create time. Empty for a run under no
            profile, a profile with no rubric, or one created before this
            field existed — the same absent-row rule every chip on this bar
            follows, so an ordinary run's header is byte-for-byte what it
            was. */}
        {run.autonomy_level && <AutonomyChip level={run.autonomy_level} />}
        {run.interactive && (
          <Chip tone="info" className="gap-1">
            <TerminalSquare className="size-3" />
            {run.state === "RUNNING" && canAttach ? "Interactive — attachable" : "Interactive"}
          </Chip>
        )}
      </div>

      {/* BarrierStrengthStrip is DECORATIVE (ConfinementChip above already
          names the tier in words) — this is what actually yields the width
          Confinement+Interactive needed back: hidden below 2xl instead of
          lg, one step later than it used to be. */}
      <div className="hidden shrink-0 2xl:block">
        <BarrierStrengthStrip tier={run.confinement_class} />
      </div>

      {/* A bare "4m 22s" beside an unlabelled clock does not say WHAT it
          measures. The title/aria-label do, without spending command-bar
          width on the word "elapsed". Hidden below 2xl — same waterfall,
          last to go: the clock is the least load-bearing fact on this bar
          (SummaryHeader's OWN clock keeps ticking; nothing reads it off
          this span). */}
      <span
        className="hidden shrink-0 items-center gap-1.5 font-mono text-xs text-muted-foreground 2xl:flex"
        title={terminal ? "Total run time" : "Time since this run started"}
        aria-label={`${terminal ? "Total run time" : "Running for"} ${elapsed}`}
      >
        <Clock className="size-3" aria-hidden="true" />
        {elapsed}
      </span>

      {/* min-w-0 so the pending-approval chip inside can actually
          shrink/truncate under pressure — a flex item's default min-width:auto
          would otherwise hold this whole group at its content width and pass
          the overflow straight on to the bar. */}
      <div className="ml-auto flex min-w-0 shrink-0 items-center gap-2">
        {/* Capped the same way as failure_hint (truncate on an
            inline-flex Chip clips with no ellipsis, so the text
            goes in its own block span). A run with real pending approvals is
            never terminal at the same time (the kill/fail cascade cancels
            them — runs_lifecycle.go), so this chip and a disabled Kill never
            actually share the bar; capped anyway, on the same reasoning as
            repo/failure_hint, so a long "N waiting · sandbox held" can never
            be the thing that pushes something ELSE off-screen. */}
        {pendingApprovalCount > 0 && (
          <Chip tone="warning" className="h-7 max-w-[130px] gap-1">
            <ShieldAlert className="size-3 shrink-0" />
            <span className="block min-w-0 truncate">
              {awaitingAdoConsent
                ? // Azure DevOps, never AWS (F13) — checked ahead of
                  // awaitingReauth, same ownership rule.
                  waitingAdoConsent(pendingApprovalCount, owned)
                : awaitingReauth
                  ? /* `owned` — the chip says whose sign-in is awaited, and only
                       the run's owner can give it (W6-U SHOULD-1). */
                    waitingReauth(pendingApprovalCount, owned)
                  : sandboxHeld
                    ? RUN_COCKPIT.waitingHeld(pendingApprovalCount)
                    : RUN_COCKPIT.waiting(pendingApprovalCount)}
            </span>
          </Chip>
        )}
        {/* Tab order clone -> kill: outline, never the header's one
            danger slot, and Kill stays disabled (not hidden) rather than
            hiding so the two sit side by side on a terminal run. */}
        {terminal && onClone && (
          <Button variant="outline" size="sm" className="h-7" onClick={onClone}>
            <RotateCcw className="size-4" /> {RUN.CLONE_CTA}
          </Button>
        )}
        <Button
          variant="outline"
          size="sm"
          // Kill is the one control on this bar that must never yield —
          // explicit shrink-0 so it is never the thing degrading away when the
          // group above it runs out of room.
          className="h-7 shrink-0 text-danger hover:text-danger"
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
    {/* SF-25: PENDING_NO_DETAIL rode only the chip's `title` above, which a
        touch device can never open — the mock (packet-4.html state 3) shows
        this sentence as a visible line under the header, not a tooltip.
        Byte-exact to PENDING_NO_DETAIL; the chip's own word stays "Queued". */}
    {pendingQueued && (
      <p data-testid="run-summary-secondary" className="border-b border-border bg-card px-4 py-1.5 text-body text-muted-foreground">
        {PENDING_NO_DETAIL}
      </p>
    )}
    </>
  );
}
