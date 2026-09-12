/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Effective-policy widget (§5c.8, C-UI) — "Effective policy" beside the
// identity rail's "Policy" row: one line per tightening launch actually
// applied, read off the run's OWN `run.create` audit row's `clamp_warnings`
// datum (C2's field) via B-γ's createRequestFromAudit — never re-derived here,
// so a member never sees a story that disagrees with the audit trail.
//
// Absent (renders nothing) when the run.create row itself is missing (an
// audit read that failed, or a trail authored before this field existed —
// clamp_warnings undefined either way, and undefined is "unknown", not "none
// tightened"). "No adjustments." (new-run-rail.tsx's own Review-rail string,
// reused rather than re-typed) only once the row affirmatively says so with an
// empty list.
import { ShieldCheck } from "lucide-react";
import type { AuditEvent } from "../../../../lib/types";
import { createRequestFromAudit } from "../../../../lib/api/audit";
import { AGENTS } from "../../../../lib/workspace-providers-copy";
import { WidgetCard } from "../../../wardyn/primitives";

export function EffectivePolicyWidget({ audit }: { audit: AuditEvent[] }) {
  const { clamp_warnings: warnings } = createRequestFromAudit(audit);
  if (warnings === undefined) return null;

  return (
    <WidgetCard title={AGENTS.EFFECTIVE_TITLE} Icon={ShieldCheck}>
      <p className="mb-2 text-xs text-muted-foreground">{AGENTS.EFFECTIVE_LEAD}</p>
      {warnings.length > 0 ? (
        <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
          {warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      ) : (
        <p className="text-xs text-muted-foreground">No adjustments.</p>
      )}
    </WidgetCard>
  );
}
