/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import { ArrowRight } from "lucide-react";
import type { AgentRun } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { rowHeadline } from "../screens/runs/board-groups";
import { AgentBadge, ConfinementChip } from "./primitives";
import { runPath, useConsoleMode } from "./console-view";

// The run an approval gates, inlined on the pending card (finding A3): the
// approver must see WHAT they're authorizing against — agent, task, repo, and
// barrier — with a click-through to the run's lifecycle hub in the same view
// (runPath: /admin/runs/:id from /admin/approvals, else /runs/:id). The run
// is fetched per-row via getRun(run_id); the queue is a handful of items so a
// per-row fetch is fine (the row stays mounted across the parent's poll because
// it's keyed by approval id, so [runId] never re-fires). A missing/gone run
// still renders the id + Open-run link so the approver can always drill in.
export function RunContextRow({
  runId,
  onRun,
}: {
  runId: string;
  /** B4: the fetched run, handed back to the card above. The approval card has
   *  to know whether the run has ENDED — a terminal run's PENDING approvals are
   *  cancelled by the lifecycle cascade, so offering Approve/Deny on one is a
   *  dead control on a governance surface — and this row already fetches
   *  exactly that record. A second getRun in the parent would be two reads of
   *  one run that can disagree. null = gone, or unreadable by this caller. */
  onRun?: (run: AgentRun | null) => void;
}) {
  // undefined = loading, null = fetch failed / run gone, AgentRun = loaded.
  const [run, setRun] = React.useState<AgentRun | null | undefined>(undefined);
  const view = useConsoleMode();
  // A ref, not a dependency: a fresh closure on every parent render must not
  // re-fire the fetch (the reason attach-terminal.tsx keeps onClose in one).
  const onRunRef = React.useRef(onRun);
  React.useEffect(() => {
    onRunRef.current = onRun;
  }, [onRun]);

  React.useEffect(() => {
    let alive = true;
    const settle = (r: AgentRun | null) => {
      if (!alive) return;
      setRun(r);
      onRunRef.current?.(r);
    };
    api
      .getRun(runId)
      .then((r) => settle(r ?? null))
      .catch(() => settle(null));
    return () => {
      alive = false;
    };
  }, [runId]);

  return (
    <Link
      to={runPath(view, runId)}
      className="group mt-3 flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-lg border border-border bg-background px-3 py-2 transition-colors hover:border-border-strong"
    >
      {run === undefined ? (
        <span className="h-5 w-48 animate-pulse rounded bg-muted" />
      ) : run ? (
        <>
          <AgentBadge agent={run.agent} withLabel={false} />
          {/* F1-F9: a local third copy of the headline chain that ignored
              run.interactive (a nameless non-interactive run read as
              "Interactive session") — the canonical helper board-groups.ts's
              rowHeadline already gets this right. false = show the title
              here, this row is not inside a titled group. */}
          <span className="min-w-0 max-w-full truncate text-sm font-medium text-foreground">
            {rowHeadline(run, false)}
          </span>
          <span className="font-mono text-xs text-muted-foreground">{run.repo}</span>
          <ConfinementChip value={run.confinement_class} />
        </>
      ) : (
        <>
          <span className="font-mono text-xs text-muted-foreground">{runId}</span>
          <span className="text-xs text-muted-foreground">run unavailable</span>
        </>
      )}
      <span className="ml-auto inline-flex items-center gap-1 whitespace-nowrap text-xs font-medium text-primary">
        Open run <ArrowRight className="size-3.5 transition-transform group-hover:translate-x-0.5" />
      </span>
    </Link>
  );
}
