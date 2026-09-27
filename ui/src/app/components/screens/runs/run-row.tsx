/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — the D2 row: one anatomy everywhere (design.md §1). A glyph, the
// title (a real link, §5), a muted meta line, the status word, and at most
// one action button. Review and Sign in both resolve to the SAME place —
// the run's own cockpit (its decision card, or the sign-in door already on
// the run header) — so the action is a second link to the run, a sibling of
// the title link rather than a nested interactive widget (same rule
// run-card.tsx's old container followed).
import { Link } from "react-router-dom";
import type { AgentRun } from "../../../lib/types";
import { relativeTime } from "../../../lib/format";
import { runPath, useConsoleMode } from "../../wardyn/console-view";
import { ownerLabel } from "../../wardyn/copy/console-view";
import { usePrincipal } from "../../wardyn/operator-context";
import { buttonVariants } from "../../ui/button";
import { cn } from "../../ui/utils";
import { repoLabel, rowHeadline } from "./board-groups";
import { glyphKindFor, RowGlyph } from "./row-glyph";
import { rowPresentation } from "./runs-model";
import { RUNS_ROW_ACTION } from "../../wardyn/copy/runs-landing";

function formatDuration(startIso: string, endIso: string): string {
  const mins = Math.max(0, Math.round((Date.parse(endIso) - Date.parse(startIso)) / 60000));
  if (mins < 60) return `${mins} min`;
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  return `${h} h ${m} min`;
}

function metaLine(run: AgentRun, adminView: boolean, own: boolean): string {
  const parts: string[] = [repoLabel(run).text];
  if (adminView) parts.push(ownerLabel(run.created_by, own));
  if (run.ended_at) {
    parts.push(`ended ${relativeTime(run.ended_at)} · ran ${formatDuration(run.created_at, run.ended_at)}`);
  } else {
    parts.push(`started ${relativeTime(run.created_at)}`);
  }
  return parts.join(" · ");
}

export function RunRow({ run }: { run: AgentRun }) {
  const view = useConsoleMode();
  const adminView = view === "admin";
  const principal = usePrincipal();
  const own = !!principal && run.created_by === principal;
  const p = rowPresentation(run, adminView);
  const glyph = glyphKindFor(p.hue, p.word, run.state);
  const title = rowHeadline(run);
  const href = runPath(view, run.id);
  const actionLabel =
    p.action === "review" ? RUNS_ROW_ACTION.REVIEW : p.action === "sign-in" ? RUNS_ROW_ACTION.SIGN_IN : null;
  const meta = metaLine(run, adminView, own);

  return (
    <div
      data-testid="run-row"
      data-attention={p.needsYou ? "needs-you" : undefined}
      className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-start gap-x-2.5 gap-y-0.5 border-t border-border px-3 py-2.5 first:border-t-0 hover:bg-surface-2/60"
    >
      <span className="pt-0.5">
        <RowGlyph hue={p.hue} kind={glyph} />
      </span>
      <div className="min-w-0">
        <Link to={href} className="block truncate text-sm font-medium text-foreground hover:underline">
          {title}
        </Link>
        <div className="truncate text-meta text-muted-foreground" title={meta}>
          {meta}
        </div>
      </div>
      <div className="flex flex-col items-end gap-1 text-right">
        <span
          className={cn(
            "whitespace-nowrap text-meta",
            p.hue === "amber" && "text-warning",
            p.hue === "red" && "text-danger",
            p.hue !== "amber" && p.hue !== "red" && "text-muted-foreground",
          )}
        >
          {p.word}
        </span>
        {/* The held-count subline (design.md §2.2) — its own line under the
            status word, always muted (the word already carries the hue). */}
        {p.subword && <span className="whitespace-nowrap text-meta text-muted-foreground">{p.subword}</span>}
        {actionLabel && (
          <Link
            to={href}
            className={cn(buttonVariants({ variant: "outline", size: "sm" }), "h-7 shrink-0")}
            onClick={(e) => e.stopPropagation()}
          >
            {actionLabel}
          </Link>
        )}
      </div>
    </div>
  );
}

export function RunRowList({ runs }: { runs: AgentRun[] }) {
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      {runs.map((run) => (
        <RunRow key={run.id} run={run} />
      ))}
    </div>
  );
}
