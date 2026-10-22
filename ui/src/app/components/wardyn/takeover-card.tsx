/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A "Needs you" request as an Approvals card (#2963, mock M8-Banner-A): the same request the run's
// banner shows, one new kind. Take over is a pointer to the run, where the person drives; the list
// is never a second place to work. Decline answers it here.
import type { ApprovalRequest } from "../../lib/types";
import { Button } from "../ui/button";
import { ApprovalKindChip, ApprovalStateBadge } from "./primitives";
import { NEEDS_YOU } from "./copy/needs-you";

/** "2m ago" — the card's own short form (M8-Banner-A "requested 2m ago"). */
export function shortAgo(ms: number): string {
  const min = Math.max(0, Math.floor(ms / 60_000));
  return `${min}m ago`;
}

export function isTakeoverRequest(a: ApprovalRequest): boolean {
  return a.kind === "human_takeover";
}

export function TakeoverCard({
  item,
  busy,
  onTakeOver,
  onDecline,
}: {
  item: ApprovalRequest;
  busy: boolean;
  onTakeOver: () => void;
  onDecline: () => void;
}) {
  const reason = String(item.requested_scope.reason ?? "");
  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="takeover-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="human_takeover" />
        <span className="text-sm font-semibold text-foreground">{NEEDS_YOU.card.title}</span>
        <span className="ml-auto">
          <ApprovalStateBadge state={item.state} />
        </span>
      </div>
      <p className="mt-3 text-sm text-muted-foreground">{NEEDS_YOU.card.asked}</p>
      <blockquote className="text-sm text-foreground">{reason}</blockquote>
      <p className="mt-2 text-sm text-muted-foreground">{NEEDS_YOU.card.blast}</p>
      {item.state === "PENDING" && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={busy} onClick={onTakeOver}>
            {NEEDS_YOU.banner.takeOver}
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={onDecline}>
            {NEEDS_YOU.banner.decline}
          </Button>
          <span className="ml-auto text-meta text-muted-foreground">
            {NEEDS_YOU.card.requested(shortAgo(Date.now() - Date.parse(item.requested_at)))}
          </span>
        </div>
      )}
    </div>
  );
}
