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

// The agent's real exit code is only ever recorded in run.complete's
// data.exit_code — nothing on AgentRun carries it, so the console reads it off
// the same audit trail the run-detail screen already holds. Mirrors the CLI's
// agentExitCode fold (cmd/wardyn/commands.go): keep the LAST event that
// actually carries a numeric code, because a later run.complete can be pure
// forensics ({"error":…}/{"panic":…}) with no code, and "the last run.complete"
// would then hide a code that WAS recorded. undefined = never recorded (killed,
// or finalized by the boot reconciler, which emits run.reconcile instead).
export function exitCodeFromAudit(events: AuditEvent[]): number | undefined {
  let code: number | undefined;
  for (const e of events) {
    if (e.action !== "run.complete") continue;
    const c = num(e.data?.exit_code);
    if (c !== undefined) code = c;
  }
  return code;
}

export const audit = {
  // GET /api/v1/audit?run_id=   (run_id optional)
  async listAudit(runId?: string): Promise<AuditEvent[]> {
    const qs = runId ? `?run_id=${encodeURIComponent(runId)}` : "";
    const res = await wfetch(withLimit(`/audit${qs}`), { method: "GET" });
    return unwrapList<AuditEvent>(await asJson<unknown>(res));
  },
};
