/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit log + the egress projection derived from audit events (the backend has
// no /egress endpoint — egress decisions are read off audit rows).
import type { AuditEvent, EgressDecision, Outcome } from "../types";
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

// The secrets-section demos widen the inline audit panel beyond egress: an
// api_key grant's mint/injection is credentialed via secret.read +
// credential.mint audit rows, which carry an OUTCOME + a secret/grant TARGET
// instead of a domain — they don't fit EgressDecision at all (audit-visibility
// note in the demos plan). DemoAuditRow is the union the secrets demos' panel
// renders; egress-section demos keep the untouched EgressDecision-only look
// (egressFromAudit, unchanged) rather than routing through this union.
export type CredentialAuditRow = {
  kind: "credential";
  id: string;
  time: string;
  action: string; // "secret.read" | "credential.mint"
  target: string;
  outcome: Outcome;
};
export type DemoAuditRow = ({ kind: "egress" } & EgressDecision) | CredentialAuditRow;

const CREDENTIAL_AUDIT_ACTIONS = new Set(["secret.read", "credential.mint"]);

export function demoAuditRows(events: AuditEvent[]): DemoAuditRow[] {
  const egressRows: DemoAuditRow[] = egressFromAudit(events).map((d) => ({ kind: "egress", ...d }));
  const credentialRows: DemoAuditRow[] = events
    .filter((e) => CREDENTIAL_AUDIT_ACTIONS.has(e.action))
    .map((e) => ({
      kind: "credential",
      id: e.id,
      time: e.time,
      action: e.action,
      target: e.target ?? "—",
      outcome: e.outcome,
    }));
  return [...egressRows, ...credentialRows];
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

// Whether the run executed a plain shell command with no agent harness. Like
// the exit code, task_mode is request-scoped and never lands on AgentRun — the
// run.create audit event is its only durable record (runs.go stamps it there
// for exactly this reason). undefined = a harness run, or an older trail.
export function taskModeFromAudit(events: AuditEvent[]): string | undefined {
  for (const e of events) {
    if (e.action !== "run.create") continue;
    const m = e.data?.task_mode;
    if (typeof m === "string" && m) return m;
  }
  return undefined;
}

export const audit = {
  // GET /api/v1/audit?run_id=&action=   (both optional; server-side filter —
  // see parseAuditFilter, internal/api/audit.go). `action` narrows the
  // 1000-row cap to just that action instead of spending the whole budget
  // on every action a chatty run logged (W21-S1-5): a run-scoped list is
  // returned OLDEST-first, so a single wide fetch can cap out before it ever
  // reaches a later action's events.
  async listAudit(runId?: string, action?: string): Promise<AuditEvent[]> {
    const params = new URLSearchParams();
    if (runId) params.set("run_id", runId);
    if (action) params.set("action", action);
    const qs = params.toString() ? `?${params.toString()}` : "";
    const res = await wfetch(withLimit(`/audit${qs}`), { method: "GET" });
    return unwrapList<AuditEvent>(await asJson<unknown>(res));
  },
};
