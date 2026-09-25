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
import { RUN_COCKPIT } from "../../../wardyn/copy";

// Fixed-width, shrink-0 rail widget — an unbounded live feed would grow it
// without limit. Same silent-cap shape as the (retired) Overview tab's Egress
// sidebar card in run-detail.tsx (`egress.slice(-6)`, no "+more" indicator):
// a rolling recent-activity view, not a paginated list.
const MAX_ROWS = 8;

export function EgressWidget({
  egress,
  heldCount = 0,
  onGoAudit,
}: {
  egress: EgressDecision[];
  /** How many approvals are HELD right now, from the run's live PENDING rows
   *  (WidgetContext.heldCount). Optional so the widget stays renderable without
   *  the cockpit above it; absent reads as ZERO.
   *
   *  B3 — this used to be `egress.filter(e => e.decision === "pending").length`,
   *  counted over an AUDIT projection. The trail is append-only, so an
   *  `egress.hold` row is a historical event that never stops being one: the
   *  chip counted every hold the run ever had and read "3 held" on a run
   *  holding nothing, on the surface whose entire job is to be the alarm. The
   *  rows below still render those events as history — it is the NUMBER that
   *  had to become state. */
  heldCount?: number;
  /** Jump to this run's Audit tab. Optional so the widget stays renderable
   *  without a parent that owns tab state (the phase-2 canvas mounts it the
   *  same way), but the parent SHOULD pass it — see the footer below. */
  onGoAudit?: () => void;
}) {
  const visible = React.useMemo(
    () => [...egress].sort((a, b) => b.time.localeCompare(a.time)).slice(0, MAX_ROWS),
    [egress],
  );

  return (
    <WidgetCard
      title="Egress"
      Icon={Globe}
      right={
        heldCount > 0 && (
          <Chip tone="warning" title={`${heldCount} held now · ${egress.length} decisions recorded`}>
            {RUN_COCKPIT.held(heldCount)}
          </Chip>
        )
      }
      bodyClassName="p-0"
    >
      {visible.length === 0 ? (
        <p className="px-2.5 py-3 text-xs text-muted-foreground">
          No outbound connections recorded yet.
        </p>
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {visible.map((e) => (
            <div key={e.id} className="flex items-center gap-2 px-2.5 py-1.5 text-xs">
              <EgressDecisionChip decision={e.decision} />
              {/* Held is drawn in full foreground (vs. muted for allow/deny) —
                  matches the board: a decision still awaiting a human reads
                  louder than settled history. */}
              <span
                className={cn(
                  "min-w-0 flex-1 truncate font-mono",
                  e.decision === "pending" ? "text-foreground" : "text-muted-foreground",
                )}
                // B3: the historical row carries the approval it raised
                // (data.approval_id), so a hold that was DECIDED later is
                // traceable from the row the chip no longer counts.
                title={e.approval_id ? `${e.domain} · approval ${e.approval_id}` : e.domain}
              >
                {e.domain}
              </span>
              <span className="shrink-0 font-mono text-meta text-muted-foreground">
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
          className="w-full border-t border-border px-2.5 py-1.5 text-left text-meta font-medium text-primary hover:underline"
        >
          Full history in Audit →
        </button>
      )}
    </WidgetCard>
  );
}
