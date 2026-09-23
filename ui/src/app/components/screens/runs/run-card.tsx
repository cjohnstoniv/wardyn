/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The board's card and the chrome around it — the grid track, the section
// heading both lanes share, and the skeleton that stands in for a card while
// the list loads.
//
// Card anatomy (mock M2), two rows and nothing else:
//   row 1  WHO (agent monogram) + WHAT (state glyph), adjacent and unfused
//          (CONSOLE-RULES §5) · headline · hover-revealed Attach · kebab
//   row 2  repo/workspace · barrier · state word · what is waiting · id · age
// Row 1 answers "who is doing what"; row 2 is the evidence line. The barrier
// does not get its own strip on row 2 — the chip already names the tier, and
// the run detail page is where the ladder is worth drawing.
import * as React from "react";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Eye, MoreHorizontal, GitBranch, RotateCcw, Skull, TerminalSquare } from "lucide-react";
import type { AgentRun } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { relativeTime, getErrorMessage } from "../../../lib/format";
import { audit as auditApi } from "../../../lib/api/audit";
import { Button } from "../../ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import { AgentBadge, ConfinementChip, RunStateBadge } from "../../wardyn/primitives";
import { usePrincipal } from "../../wardyn/operator-context";
import { RunStateGlyph } from "../../wardyn/run-state-glyph";
import { KillRunDialog } from "../../wardyn/kill-run-dialog";
import { RUN, RUN_COCKPIT } from "../../wardyn/copy";
// THE LEAF, not wardyn/model-access-copy: this card is on the eager graph and
// that module is lazy-side (see lib/reauth-waiting-copy.ts).
import { waitingAdoConsent, waitingReauth } from "../../../lib/reauth-waiting-copy";
import { Mono } from "../../wardyn/code-block";
import { cn } from "../../ui/utils";
import { repoLabel, rowHeadline, runAttention, shortId, signalsFor, type RunSignals } from "./board-groups";
// review U-01: cloneFromAudit + CLONE_UNREADABLE moved to wizard-types.ts so
// this door and the run header's (run-detail.tsx onClone) share ONE refusal
// path and ONE string, rather than reimplementing the same guard twice.
import { cloneFromAudit, CLONE_LOAD_FAILED, CLONE_UNREADABLE } from "../new-run/wizard-types";
import { statusDetailSentence } from "../run-status-detail";

export function CardGrid({ children }: { children: React.ReactNode }) {
  // auto-fill with a min(100%, floor) track: cards reflow and collapse to ONE
  // column below the floor instead of clipping (min(100%, …) stops overflow on
  // narrow containers). The floor is 26rem: 34rem would yield two columns on a
  // 1400px board where three fit comfortably now that the card is two rows
  // instead of four.
  return (
    <div className="grid gap-3 grid-cols-[repeat(auto-fill,minmax(min(100%,26rem),1fr))]">{children}</div>
  );
}

export function SectionHeading({
  Icon,
  title,
  count,
  tone = "neutral",
}: {
  Icon?: React.ElementType;
  title: string;
  count: number;
  /** The Needs-you lane owns amber; every other heading is quiet. One
   *  heading has one tone — not iconTint/titleTint/countTint, since no
   *  caller ever needs them to differ. */
  tone?: "neutral" | "warning";
}) {
  const warn = tone === "warning";
  return (
    <div className="mb-3 flex items-center gap-2">
      {Icon && <Icon className={cn("size-3.5", warn && "text-warning")} />}
      {/* §3: the 11px uppercase rung is `.label-eyebrow` (0.06em), not a
          hand-rolled `tracking-wider` that disagrees with it by 0.01em.
          It hard-codes --muted-foreground and is emitted AFTER Tailwind's
          colour utilities in the same @layer, so the amber variant has to
          carry `!` to win the cascade — without it the lane heading renders
          grey. */}
      <h2 className={cn("label-eyebrow", warn && "text-warning!")}>{title}</h2>
      <span
        className={cn(
          "rounded-full px-1.5 text-meta font-semibold",
          warn ? "bg-warning-subtle text-warning" : "bg-muted text-muted-foreground",
        )}
      >
        {count}
      </span>
    </div>
  );
}

export function RunCard({
  run,
  signals,
  grouped,
  onOpen,
  onKill,
}: {
  run: AgentRun;
  /** Live approval facts for the whole board; this card reads its own row. */
  signals: RunSignals;
  /** This card sits under a shared-title header, so it names its own work
   *  rather than repeating the title — see rowHeadline. */
  grouped?: boolean;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  const s = signalsFor(run, signals);
  // The viewer, for the one sentence on this card that is audience-dependent:
  // whose AWS sign-in a held run is waiting on. usePrincipal()'s default is ""
  // — "not mine" — which is the fail-closed direction for this comparison.
  const principal = usePrincipal();
  const attention = runAttention(run, signals);
  const terminal = isTerminalRunState(run.state);
  const done = terminal;
  const attachable = !!run.interactive && run.state === "RUNNING";
  // An approval is a request and a failure is a report, so they no longer share
  // one amber treatment: the request gets the warning rail (and pins to the
  // Needs-you lane), the report gets a danger rail beside its own work.
  const asks = attention === "permission";
  const interrupted = attention === "interrupted";
  const repo = repoLabel(run);

  // This container is a plain <div>, not role="button" tabIndex={0}: that
  // would be a widget role directly nesting the real Attach/Review/kebab
  // <button>s below, an invalid ARIA structure (interactive-in-interactive).
  // Mouse click-to-open stays via the plain onClick; keyboard/AT users
  // already have a dedicated affordance for the same action (RunActions'
  // "Open detail" menu item), so no functionality is lost by dropping the
  // role.
  return (
    <div
      // A stable e2e hook: a class-based xpath locator would couple the spec
      // to a Tailwind utility name.
      data-testid="run-card"
      onClick={() => onOpen(run.id)}
      className={cn(
        "group relative flex cursor-pointer flex-col gap-2 rounded-xl border bg-card p-3 text-left transition-colors hover:border-border-strong",
        asks ? "border-warning/30" : interrupted ? "border-danger/30" : "border-border",
        done && "opacity-90",
      )}
    >
      {(asks || interrupted) && (
        <span
          className={cn(
            "absolute inset-y-3 left-0 w-0.5 rounded-r",
            asks ? "bg-warning" : "bg-danger",
          )}
          aria-hidden="true"
        />
      )}

      {/* Row 1 — who + what, then the name of the work. */}
      <div className="flex items-center gap-2">
        <AgentBadge agent={run.agent} withLabel={false} />
        <RunStateGlyph state={run.state} signals={s} />
        {/* #215 — a real <a href>, not a div with onClick: no anchor, no
            role, no tabIndex meant this could not be reached by keyboard,
            middle-clicked, opened in a new tab, or copied as a link. The
            card's own onClick below still opens it by mouse anywhere else on
            the card; stopPropagation here just keeps the click from firing
            twice. */}
        <Link
          to={`/runs/${encodeURIComponent(run.id)}`}
          onClick={(e) => e.stopPropagation()}
          className="min-w-0 flex-1 truncate text-body font-medium leading-snug text-foreground hover:underline"
        >
          {rowHeadline(run, !!grouped)}
        </Link>
        {/* Review/Open is the point of a card that needs you, so it never
            hides. Attach is a convenience on a healthy run — revealed on
            hover, and on focus-within so it is reachable by keyboard, never
            hover-only. #215: a failed run's "Review" becomes "Open" — it is
            a report, not a request, and the two no longer share one word. */}
        {(asks || interrupted) && (
          <Button
            size="sm"
            // §6: exactly one `default` button per surface, and the board's is
            // the shell's New run. A lane of N asking cards would be N teal
            // buttons competing with it — the amber rail and the pinned lane
            // already say which cards are the request, so this one only has
            // to be reachable.
            variant="outline"
            className="h-7 shrink-0"
            onClick={(e) => {
              e.stopPropagation();
              onOpen(run.id);
            }}
          >
            {interrupted ? "Open" : "Review"}
          </Button>
        )}
        {attachable && (
          <Button
            size="sm"
            variant="outline"
            className="h-7 shrink-0 opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100"
            onClick={(e) => {
              e.stopPropagation();
              onOpen(run.id);
            }}
          >
            <TerminalSquare className="size-3.5" /> Attach
          </Button>
        )}
        <RunActions run={run} terminal={terminal} attachable={attachable} onOpen={onOpen} onKill={onKill} />
      </div>

      {/* Row 2 — the evidence line, one 11px rung throughout. */}
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-meta text-muted-foreground">
        <GitBranch className="size-3.5 shrink-0" aria-hidden="true" />
        <span
          className={cn("min-w-0 truncate", repo.mono ? "font-mono" : "italic")}
          title={run.workspace_path || run.repo || undefined}
        >
          {repo.text}
        </span>
        <ConfinementChip value={run.confinement_class} />
        <RunStateBadge state={run.state} variant="label" />
        {/* 0.7.6 finding 6: the same reason the table row carries, so a person
            reading the board in card mode is not the one left guessing. */}
        {statusDetailSentence(run.status_detail, run.status_reason) && (
          <span className="min-w-0 truncate text-muted-foreground">
            {statusDetailSentence(run.status_detail, run.status_reason)}
          </span>
        )}
        {/* A held approval says what is waiting; a failure says nothing extra —
            the glyph and Review already carry it, and a sentence repeating the
            state was three words of noise on every attention card. */}
        {s.pending > 0 && (
          <span className="whitespace-nowrap text-warning">
            {s.adoConsent
              ? // Azure DevOps, never AWS (F13) — checked ahead of s.reauth,
                // same "whose sign-in" ownership rule.
                waitingAdoConsent(s.pending, !!principal && run.created_by === principal)
              : s.reauth
                ? /* Whose sign-in — the board shows an admin every run, and a
                     member the shared-lane rows their own runs raised (W6-U
                     SHOULD-1). An unresolved /me reads as "not mine", the same
                     fail-closed direction the cockpit's door takes. */
                  waitingReauth(s.pending, !!principal && run.created_by === principal)
                : s.held
                  ? RUN_COCKPIT.waitingHeld(s.pending)
                  : RUN_COCKPIT.waiting(s.pending)}
          </span>
        )}
        <span className="ml-auto flex items-center gap-2.5">
          <Mono className="max-w-[8rem] truncate text-meta" title={run.id}>
            {shortId(run.id)}
          </Mono>
          <span className="whitespace-nowrap" title={run.created_at}>
            {relativeTime(run.created_at)}
          </span>
        </span>
      </div>
    </div>
  );
}

export function RunActions({
  run,
  terminal,
  attachable,
  onOpen,
  onKill,
}: {
  run: AgentRun;
  terminal: boolean;
  attachable: boolean;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  // The board's Kill action needs confirmation, like the identical action on
  // Run Detail — without it, one misclick kills a run with zero chance to
  // back out. The dialog is rendered as a SIBLING of DropdownMenuContent (not
  // nested inside it), controlled by its own state, so it survives the
  // menu's close/unmount.
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  const navigate = useNavigate();
  // 0.7.3 F7 — the Runs-list door onto the same clone the run header offers
  // (byte-for-byte: task_mode / interactive_start / seed_auto_tools /
  // tool_approvals all come from the run.create audit row). Fetched on CLICK,
  // never per row render — the board can hold dozens of terminal runs and
  // only one of them is ever cloned at a time. Narrowed to the one action
  // createRequestFromAudit reads (review C-07) — no reason to spend the
  // 1000-row budget on the whole trail for one row.
  // ponytail: no pending/disabled state on the menu item while this fetches —
  // it is one indexed Postgres read, typically faster than the menu's own
  // close animation. Add a dismissed toast.loading if users ever report a
  // dead-feeling click.
  const cloneRun = async () => {
    try {
      const events = await auditApi.listAudit(run.id, "run.create");
      // review C-01/C-06/U-01: the SAME refusal the run header uses — an
      // empty read (an older run, a pruned trail, or a non-owner's empty
      // 200 — auditScope writes an empty list rather than an error) must
      // not silently launch a clone with wizard DEFAULTS standing in for
      // task_mode/interactive_start/seed_auto_tools/tool_approvals.
      const prefill = cloneFromAudit(run, events);
      if (!prefill) {
        toast.warning(CLONE_UNREADABLE);
        return;
      }
      navigate("/runs/new", { state: { prefill } });
    } catch (err) {
      toast.error(CLONE_LOAD_FAILED, { description: getErrorMessage(err) });
    }
  };
  return (
    // This guard is RunCard's only defense (the board is the default Mode —
    // RunCard has no TableCell of its own to also carry it, unlike the table
    // row): onClick-only let Enter/Space on the kebab reach RunCard's
    // row-level onKeyDown and navigate instead of opening the menu.
    <div onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" className="size-8" aria-label="Run actions">
            <MoreHorizontal className="size-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={() => onOpen(run.id)}>
            <Eye className="size-4" /> Open detail
          </DropdownMenuItem>
          {attachable && (
            <DropdownMenuItem onClick={() => onOpen(run.id)}>
              <TerminalSquare className="size-4" /> Attach
            </DropdownMenuItem>
          )}
          {terminal && (
            <DropdownMenuItem onClick={cloneRun}>
              <RotateCcw className="size-4" /> {RUN.CLONE_CTA}
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            disabled={terminal}
            onClick={() => setConfirmId(run.id)}
            className="text-danger focus:text-danger"
          >
            <Skull className="size-4" /> Kill run
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <KillRunDialog
        runId={confirmId}
        onOpenChange={(o) => !o && setConfirmId(null)}
        onConfirm={() => onKill(run.id)}
      />
    </div>
  );
}

// Skeletons match the final layout's height (CONSOLE-RULES §9): two rows in a
// p-3 card, not a four-row p-4 block — an over-tall skeleton makes the board
// jump the moment the list lands.
export function BoardSkeleton() {
  return (
    <div className="grid gap-3 grid-cols-[repeat(auto-fill,minmax(min(100%,26rem),1fr))]">
      {Array.from({ length: 6 }).map((_, i) => (
        <div key={i} className="space-y-2 rounded-xl border border-border bg-card p-3">
          <div className="flex items-center gap-2">
            <div className="size-6 shrink-0 animate-pulse rounded-full bg-muted" />
            <div className="h-3.5 w-full animate-pulse rounded bg-muted" />
          </div>
          <div className="flex items-center gap-2">
            <div className="h-4 w-28 animate-pulse rounded bg-muted" />
            <div className="h-4 w-16 animate-pulse rounded bg-muted" />
            <div className="ml-auto h-3 w-20 animate-pulse rounded bg-muted" />
          </div>
        </div>
      ))}
    </div>
  );
}
