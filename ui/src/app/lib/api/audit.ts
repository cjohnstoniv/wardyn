/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit log + the egress projection derived from audit events (the backend has
// no /egress endpoint — egress decisions are read off audit rows).
import type { AuditEvent, EgressDecision } from "../types";
import { asJson, num, str, unwrapList, wfetch, withLimit } from "./core";

// Project egress.allow / egress.deny / egress.pending audit events into
// the EgressDecision shape the run-detail screen renders. Exported so callers
// that already hold a run's audit events can derive egress WITHOUT a second
// /audit round-trip.
export function egressFromAudit(events: AuditEvent[]): EgressDecision[] {
  const map: Record<string, "allow" | "deny" | "pending"> = {
    "egress.allow": "allow",
    "egress.deny": "deny",
    "egress.pending": "pending",
  };
  return events
    .filter((e) => e.action in map)
    .map((e) => {
      const d = (e.data ?? {}) as Record<string, unknown>;
      // Prefer an explicit domain in data; otherwise strip a :port off target.
      const domain =
        str(d.domain) ?? (e.target ? e.target.replace(/:\d+$/, "") : "—");
      return {
        id: e.id,
        time: e.time,
        domain,
        decision: map[e.action],
        bytes: num(d.bytes),
      } satisfies EgressDecision;
    });
}

export const audit = {
  // GET /api/v1/audit?run_id=   (run_id optional)
  async listAudit(runId?: string): Promise<AuditEvent[]> {
    const qs = runId ? `?run_id=${encodeURIComponent(runId)}` : "";
    const res = await wfetch(withLimit(`/audit${qs}`), { method: "GET" });
    return unwrapList<AuditEvent>(await asJson<unknown>(res));
  },
};
