/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import { ArrowRight } from "lucide-react";
import type { AgentRun } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { AgentBadge, ConfinementChip } from "./primitives";

// The run an approval gates, inlined on the pending card (finding A3): the
// approver must see WHAT they're authorizing against — agent, task, repo, and
// barrier — with a click-through to the run's lifecycle hub at /runs/:id. The run
// is fetched per-row via getRun(run_id); the queue is a handful of items so a
// per-row fetch is fine (the row stays mounted across the parent's poll because
// it's keyed by approval id, so [runId] never re-fires). A missing/gone run
// still renders the id + Open-run link so the approver can always drill in.
export function RunContextRow({ runId }: { runId: string }) {
  // undefined = loading, null = fetch failed / run gone, AgentRun = loaded.
  const [run, setRun] = React.useState<AgentRun | null | undefined>(undefined);

  React.useEffect(() => {
    let alive = true;
    api
      .getRun(runId)
      .then((r) => alive && setRun(r ?? null))
      .catch(() => alive && setRun(null));
    return () => {
      alive = false;
    };
  }, [runId]);

  return (
    <Link
      to={`/runs/${encodeURIComponent(runId)}`}
      className="group mt-3 flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-lg border border-border bg-background px-3 py-2 transition-colors hover:border-border-strong"
    >
      {run === undefined ? (
        <span className="h-5 w-48 animate-pulse rounded bg-muted" />
      ) : run ? (
        <>
          <AgentBadge agent={run.agent} withLabel={false} />
          <span className="min-w-0 max-w-full truncate text-sm font-medium text-foreground">
            {run.task || "Interactive session"}
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
