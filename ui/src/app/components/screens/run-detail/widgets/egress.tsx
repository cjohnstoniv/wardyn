/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Egress widget — the run cockpit's evidence-rail row for outbound connection
// decisions. `egress` is already derived by the PARENT (egressFromAudit over
// the same /audit fetch the rest of the rail shares) — this widget never
// fetches on its own, so the rail can't race N independent audit reads for
// the same data.
import * as React from "react";
import { Globe } from "lucide-react";
import type { EgressDecision } from "../../../../lib/types";
import { relativeTime } from "../../../../lib/format";
import { cn } from "../../../ui/utils";
import { Chip, EgressDecisionChip, WidgetCard } from "../../../wardyn/primitives";

// Fixed-width, shrink-0 rail widget — an unbounded live feed would grow it
// without limit. Same silent-cap shape as the (retired) Overview tab's Egress
// sidebar card in run-detail.tsx (`egress.slice(-6)`, no "+more" indicator):
// a rolling recent-activity view, not a paginated list.
const MAX_ROWS = 8;

export function EgressWidget({
  egress,
  onGoAudit,
}: {
  egress: EgressDecision[];
  /** Jump to this run's Audit tab. Optional so the widget stays renderable
   *  without a parent that owns tab state (the phase-2 canvas mounts it the
   *  same way), but the parent SHOULD pass it — see the footer below. */
  onGoAudit?: () => void;
}) {
  const heldCount = egress.filter((e) => e.decision === "pending").length;
  const visible = React.useMemo(
    () => [...egress].sort((a, b) => b.time.localeCompare(a.time)).slice(0, MAX_ROWS),
    [egress],
  );

  return (
    <WidgetCard
      title="Egress"
      Icon={Globe}
      right={heldCount > 0 && <Chip tone="warning">{heldCount} held</Chip>}
      bodyClassName="p-0"
    >
      {visible.length === 0 ? (
        <p className="px-2.5 py-3 text-[0.75rem] text-muted-foreground">
          No outbound connections recorded yet.
        </p>
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {visible.map((e) => (
            <div key={e.id} className="flex items-center gap-2 px-2.5 py-1.5 text-[0.75rem]">
              <EgressDecisionChip decision={e.decision} />
              {/* Held is drawn in full foreground (vs. muted for allow/deny) —
                  matches the board: a decision still awaiting a human reads
                  louder than settled history. */}
              <span
                className={cn(
                  "min-w-0 flex-1 truncate font-mono",
                  e.decision === "pending" ? "text-foreground" : "text-muted-foreground",
                )}
                title={e.domain}
              >
                {e.domain}
              </span>
              <span className="shrink-0 font-mono text-[0.625rem] text-muted-foreground">
                {relativeTime(e.time)}
              </span>
            </div>
          ))}
        </div>
      )}
      {/* The cap above is a rolling window, so there has to be a way OUT of it.
          The Overview card this widget replaced carried the same "Full history
          in Audit →" link; dropping it would have left the cap silent and
          terminal — an operator who can see 8 of 40 decisions and no path to
          the rest. Rendered only when there is more than fits, so a short run
          gets no dead affordance. */}
      {onGoAudit && egress.length > visible.length && (
        <button
          onClick={onGoAudit}
          className="w-full border-t border-border px-2.5 py-1.5 text-left text-[0.6875rem] font-medium text-primary hover:underline"
        >
          Full history in Audit →
        </button>
      )}
    </WidgetCard>
  );
}
