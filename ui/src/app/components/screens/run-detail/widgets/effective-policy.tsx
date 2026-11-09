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
// tightened"). The registry carries the SAME predicate as `available`, so the
// canvas reserves no tile for a null. AGENTS.EFFECTIVE_NONE — the canon key the
// New Run rail's preflight block reads too — only once the row affirmatively
// says so with an empty list.
import { ShieldCheck } from "lucide-react";
import type { AuditEvent } from "../../../../lib/types";
import { createRequestFromAudit } from "../../../../lib/api/audit";
import { AGENTS } from "../../../../lib/workspace-providers-copy";
import { WidgetCard } from "../../../wardyn/primitives";

// The hosts whose credentials the run was started or revived without because its policy did not allow
// them exactly or a wall denied them: the dropped_injection_hosts of its run.ceiling.reassert and run.revive rows, in the order
// first seen. Host names only.
export function withheldCredentialHosts(audit: AuditEvent[]): string[] {
  const hosts: string[] = [];
  for (const e of audit) {
    if (e.action !== "run.ceiling.reassert" && e.action !== "run.revive") continue;
    const dropped = (e.data as Record<string, unknown> | undefined)?.dropped_injection_hosts;
    if (!Array.isArray(dropped)) continue;
    for (const h of dropped) {
      if (typeof h === "string" && h !== "" && !hosts.includes(h)) hosts.push(h);
    }
  }
  return hosts;
}

export function EffectivePolicyWidget({ audit }: { audit: AuditEvent[] }) {
  const { clamp_warnings: warnings } = createRequestFromAudit(audit);
  const withheld = withheldCredentialHosts(audit);
  if (warnings === undefined && withheld.length === 0) return null;

  return (
    <WidgetCard title={AGENTS.EFFECTIVE_TITLE} Icon={ShieldCheck}>
      <p className="mb-2 text-xs text-muted-foreground">{AGENTS.EFFECTIVE_LEAD}</p>
      {warnings === undefined ? null : warnings.length > 0 ? (
        <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
          {warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      ) : (
        <p className="text-xs text-muted-foreground">{AGENTS.EFFECTIVE_NONE}</p>
      )}
      {withheld.length > 0 && (
        <p className="mt-2 text-xs text-warning">
          {`credentials withheld for ${withheld.join(", ")}: the run's policy does not allow these hosts exactly, or a wall denies them; the run's run.ceiling.reassert and run.revive audit rows say which`}
        </p>
      )}
    </WidgetCard>
  );
}
