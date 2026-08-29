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
// STRIP that used to sit on row 2 moved out entirely — the chip already names
// the tier, and the run detail page is where the ladder is worth drawing.
import * as React from "react";
import { Eye, MoreHorizontal, GitBranch, Skull, TerminalSquare } from "lucide-react";
import type { AgentRun } from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { relativeTime } from "../../../lib/format";
import { Button } from "../../ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import { AgentBadge, ConfinementChip, RunStateBadge } from "../../wardyn/primitives";
import { RunStateGlyph } from "../../wardyn/run-state-glyph";
import { KillRunDialog } from "../../wardyn/kill-run-dialog";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { Mono } from "../../wardyn/code-block";
import { cn } from "../../ui/utils";
import { repoLabel, rowHeadline, runAttention, shortId, signalsFor, type RunSignals } from "./board-groups";

export function CardGrid({ children }: { children: React.ReactNode }) {
  // auto-fill with a min(100%, floor) track: cards reflow and collapse to ONE
  // column below the floor instead of clipping (min(100%, …) stops overflow on
  // narrow containers). The floor is 26rem, not the 34rem this shipped with —
  // 34 yields two columns on a 1400px board where three fit comfortably now
  // that the card is two rows instead of four.
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
  /** The Needs-you lane owns amber; every other heading is quiet. This was
   *  three props (iconTint / titleTint / countTint) that no caller ever set
   *  to different values — one heading has one tone. */
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

  // fix: this container used to be role="button" tabIndex={0} — a widget
  // role directly nesting the real Attach/Review/kebab <button>s below,
  // which is an invalid ARIA structure (interactive-in-interactive). Mouse
  // click-to-open stays via the plain onClick; keyboard/AT users already
  // have a dedicated affordance for the same action (RunActions' "Open
  // detail" menu item), so no functionality is lost by dropping the role.
  return (
    <div
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
        <p className="min-w-0 flex-1 truncate text-body font-medium leading-snug text-foreground">
          {rowHeadline(run, !!grouped)}
        </p>
        {/* Review is the point of a card that needs you, so it never hides.
            Attach is a convenience on a healthy run — revealed on hover, and
            on focus-within so it is reachable by keyboard, never hover-only. */}
        {(asks || interrupted) && (
          <Button
            size="sm"
            // §6: exactly one `default` button per surface, and the board's is
            // the shell's New run. A lane of N asking cards was N teal buttons
            // competing with it — the amber rail and the pinned lane already
            // say which cards are the request, so this one only has to be
            // reachable.
            variant="outline"
            className="h-7 shrink-0"
            onClick={(e) => {
              e.stopPropagation();
              onOpen(run.id);
            }}
          >
            Review
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
        {/* A held approval says what is waiting; a failure says nothing extra —
            the glyph and Review already carry it, and a sentence repeating the
            state was three words of noise on every attention card. */}
        {s.pending > 0 && (
          <span className="whitespace-nowrap text-warning">
            {s.held ? RUN_COCKPIT.waitingHeld(s.pending) : RUN_COCKPIT.waiting(s.pending)}
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
  // fix: the board's Kill action fired with no confirmation, unlike the
  // identical action on Run Detail — one misclick here killed a run with zero
  // chance to back out. The dialog is rendered as a SIBLING of
  // DropdownMenuContent (not nested inside it), controlled by its own state, so
  // it survives the menu's close/unmount.
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
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
// p-3 card, not the four-row p-4 block the card used to be — an over-tall
// skeleton makes the board jump the moment the list lands.
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
