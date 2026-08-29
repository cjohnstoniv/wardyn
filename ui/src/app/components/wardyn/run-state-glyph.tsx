// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

import { Archive, Check, CircleHelp, Loader2, Radio, ShieldX, Square } from "lucide-react";
import type { RunState } from "../../lib/types/runs";
import { cn } from "../ui/utils";

/**
 * The "what" half of a run's identity glyph pair (docs/design/CONSOLE-RULES.md §5):
 * rendered next to the "who" (the agent badge), never fused with it. One attention
 * vocabulary for the runs board and the cockpit header, ranked so that when two
 * indicators compete on one row the more urgent one wins.
 */
export type RunAttention =
  | "permission" // a human must answer: held egress/tool approval, awaiting confirmation
  | "monitoring" // a pending approval that does not block the run (deny-with-review)
  | "interrupted" // FAILED / KILLED — evidence needing review
  | "working" // RUNNING and streaming
  | "active" // RUNNING, idle
  | "starting"
  | "queued" // PENDING
  | "done"
  | "inactive"; // STOPPED / ARCHIVED

/** Lower rank = more urgent. Ranks 1–3 are "needs you". */
const RANK: Record<RunAttention, number> = {
  permission: 1,
  interrupted: 2,
  monitoring: 3,
  working: 4,
  active: 5,
  starting: 5,
  queued: 6,
  done: 7,
  inactive: 8,
};

export function attentionRank(a: RunAttention): number {
  return RANK[a];
}

export type AttentionSignals = {
  /** A held approval (wait_for_review) is parked on this run. */
  held?: boolean;
  /** A pending approval that lets the run continue (deny_with_review). */
  passiveHold?: boolean;
  /** Someone is attached / output is streaming. */
  working?: boolean;
};

/** Maps a run state plus live signals onto the attention vocabulary. */
export function attentionFor(state: RunState | string, s: AttentionSignals = {}): RunAttention {
  if (s.held || state === "WAITING_FOR_CONFIRMATION") return "permission";
  if (state === "FAILED" || state === "KILLED") return "interrupted";
  if (s.passiveHold && state === "RUNNING") return "monitoring";
  switch (state) {
    case "RUNNING":
      return s.working ? "working" : "active";
    case "STARTING":
      return "starting";
    case "PENDING":
      return "queued";
    case "COMPLETED":
      return "done";
    default:
      return "inactive";
  }
}

export const ATTENTION_LABEL: Record<RunAttention, string> = {
  permission: "Needs you",
  interrupted: "Needs review",
  monitoring: "Approval pending",
  working: "Working",
  active: "Running",
  starting: "Starting",
  queued: "Queued",
  done: "Done",
  inactive: "Stopped",
};

/**
 * The glyph itself: an icon or a dot, coloured by semantic tone only (never the
 * teal accent, never a barrier METAL). Reduced motion freezes the working ring.
 */
export function RunStateGlyph({
  state,
  signals,
  className,
}: {
  state: RunState | string;
  signals?: AttentionSignals;
  className?: string;
}) {
  const a = attentionFor(state, signals);
  const label = ATTENTION_LABEL[a];
  const common = { "data-attention": a, "aria-label": label, title: label, role: "img" as const };
  const icon = "size-3.5 shrink-0";
  switch (a) {
    case "permission":
      return <CircleHelp {...common} className={cn(icon, "text-warning", className)} />;
    case "monitoring":
      return <Radio {...common} className={cn(icon, "text-warning/70", className)} />;
    case "interrupted":
      return state === "KILLED" ? (
        <ShieldX {...common} className={cn(icon, "text-danger", className)} />
      ) : (
        <span {...common} className={cn("inline-block size-2 shrink-0 rounded-full bg-danger", className)} />
      );
    case "working":
      return (
        <Loader2
          {...common}
          className={cn(icon, "animate-spin text-success motion-reduce:animate-none", className)}
        />
      );
    case "active":
      return <span {...common} className={cn("inline-block size-2 shrink-0 rounded-full bg-success", className)} />;
    case "starting":
      return <span {...common} className={cn("inline-block size-2 shrink-0 rounded-full bg-info", className)} />;
    case "queued":
      return (
        <span
          {...common}
          className={cn("inline-block size-2 shrink-0 rounded-full border border-muted-foreground/60", className)}
        />
      );
    case "done":
      return <Check {...common} className={cn(icon, "text-success", className)} />;
    default:
      return state === "ARCHIVED" ? (
        <Archive {...common} className={cn(icon, "text-muted-foreground", className)} />
      ) : (
        <Square {...common} className={cn(icon, "text-muted-foreground", className)} />
      );
  }
}
