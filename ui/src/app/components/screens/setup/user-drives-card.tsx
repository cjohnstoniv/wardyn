/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The user-drives card — ONE component in two homes (user-drives-mock state 9):
// the setup funnel's `workspaces` step, under the workspace list, and Settings,
// as a fifth card. It lives here rather than in either caller for the reason
// Settings' other shared cards do: two copies of a summary is how the two
// surfaces start disagreeing about the same rows.
//
// ZERO TEAL, both places. The funnel's footer Next is the step's one
// affirmative action and Settings' cards delegate, so this is a two-line link
// button into /drives — never a form, and never a second write. It counts
// ALLOCATIONS, never people: a group allocation is one row and Wardyn holds no
// directory read, so "14 people" would be a claim rather than a count.
//
// SUPER only. A security admin's authority over drives is the governance
// profile's door, not the registry, so they see no card at all — the same
// reason /drives has no nav item.
//
// Every product string comes from user-drives-copy.ts. This file adds none.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { drives as api } from "../../../lib/api/drives";
import { DRIVES } from "../../../lib/user-drives-copy";
import { useOperator } from "../../wardyn/operator-context";
import { CollapsibleCard } from "../../wardyn/collapsible-card";

export function UserDrivesCard({
  /** #1200 compact cards — Settings' only. Getting started's `workspaces`
   *  step renders this same component and must stay fully open, so the
   *  collapse is an opt-in, never this component's own new default. */
  compact = false,
}: { compact?: boolean } = {}) {
  const operator = useOperator();
  const navigate = useNavigate();
  const [counts, setCounts] = React.useState<{ drives: number; allocations: number } | null>(null);

  React.useEffect(() => {
    if (!operator) return;
    let live = true;
    api
      .getDrives()
      .then((s) => live && setCounts({ drives: s.drives.length, allocations: s.grants.length }))
      // A failed read leaves the summary ABSENT rather than claiming "No drives
      // yet." — a confident empty state over an unloaded snapshot is a false
      // claim, and the link still goes where the real list is.
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [operator]);

  if (!operator) return null;

  const summary = !counts
    ? ""
    : counts.drives === 0
      ? DRIVES.CARD_EMPTY
      : DRIVES.CARD_SUMMARY(DRIVES.CARD_DRIVES(counts.drives), DRIVES.CARD_ALLOCATIONS(counts.allocations));

  // #1200 compact cards: compact mode already states `summary` in the
  // collapsed header, so the open-button's own copy of it would duplicate
  // the same text node twice once expanded — suppressed there, unchanged
  // (still the two-line button) in Getting started.
  const openButton = (
    <button
      type="button"
      onClick={() => navigate("/admin/drives")}
      className="mt-3 flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left transition-colors hover:border-border-strong"
    >
      <span>
        <span className="block text-body font-medium text-foreground">{DRIVES.CARD_OPEN}</span>
        {summary && !compact && <span className="block text-meta text-muted-foreground">{summary}</span>}
      </span>
      <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
    </button>
  );

  if (compact) {
    return (
      <CollapsibleCard title={DRIVES.TITLE} summary={summary || undefined} testId="user-drives-card">
        <p className="text-body leading-snug text-muted-foreground">{DRIVES.CARD_LEAD}</p>
        {openButton}
      </CollapsibleCard>
    );
  }
  return (
    <section className="rounded-xl border border-border bg-card p-4" data-testid="user-drives-card">
      <h3 className="text-sm font-medium text-foreground">{DRIVES.TITLE}</h3>
      <p className="mt-0.5 text-body leading-snug text-muted-foreground">{DRIVES.CARD_LEAD}</p>
      {openButton}
    </section>
  );
}
