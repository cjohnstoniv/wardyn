/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit log + the egress projection derived from audit events (the backend has
// no /egress endpoint — egress decisions are read off audit rows).
import type { AuditEvent, EgressDecision, Outcome, RunEnding, RunEndingKind, RunState } from "../types";
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

// Which audit action is the ROOT cause of a FAILED run, in the order a run
// hits them. Read as: the sandbox image, then the image's own selftest. Any
// other failing action stays "unknown" on purpose — run.dispatch/failure alone
// covers a concurrent kill, a sandbox-create error and a teardown error, and
// naming one of those as the cause would be a guess dressed as a diagnosis.
const FAILED_CAUSE: Record<string, RunEndingKind> = {
  "run.build": "image",
  "run.selftest": "selftest",
};

/** The FIRST event matching `pick` — the root cause, not the last symptom. */
function firstEvent(events: AuditEvent[], pick: (e: AuditEvent) => boolean): AuditEvent | undefined {
  return events.find(pick);
}

/** The LAST event matching `pick` — the most recent attempt, not the first. */
function lastEvent(events: AuditEvent[], pick: (e: AuditEvent) => boolean): AuditEvent | undefined {
  return events.filter(pick).pop();
}

// Why a run ended badly, from the trail the run page already holds. The console
// half of the CLI's runFailureReason (cmd/wardyn/commands.go), which scans the
// same events FORWARD for the first failure — the earliest failure is the
// cause; everything after it is fallout.
//
// The run STATE picks the family and the audit picks the specifics, because
// the two answer different questions: KILLED is never a build problem, and a
// STOPPED run is only worth explaining when the reaper stopped it (a run
// someone stopped on purpose is not a failure and gets no block at all).
// Returns undefined for every state that ended fine — the block must not
// appear on a run that completed.
export function runEndingFromAudit(state: RunState, events: AuditEvent[]): RunEnding | undefined {
  const from = (kind: RunEndingKind, e: AuditEvent | undefined, action: string): RunEnding => ({
    kind,
    action: e?.action ?? action,
    outcome: e?.outcome,
    actor: e?.actor,
    time: e?.time,
    detail: str(e?.data?.error) ?? str(e?.data?.reason) ?? str(e?.data?.detail),
  });
  if (state === "KILLED") {
    // The kill event carries WHO and WHEN. The LAST one, not the first: the
    // server exempts an already-KILLED run from the terminal guard precisely so
    // a kill whose teardown failed can be retried (runs_lifecycle.go), so the
    // most recent attempt — not the first — is this run's containment truth. A
    // KILLED run with no run.kill row at all (an older trail, or one truncated
    // by the 1000-row cap) still gets the block: the state alone is the fact,
    // only the attribution is missing.
    return from("killed", lastEvent(events, (e) => e.action === "run.kill"), "run.kill");
  }
  if (state === "STOPPED") {
    const stop = firstEvent(events, (e) => e.action === "run.autostop");
    return stop ? from("auto_stop", stop, "run.autostop") : undefined;
  }
  if (state !== "FAILED") return undefined;
  // fail_closed:false is a WARN-ONLY selftest — an interactive BYOI run runs it
  // for the warning and carries on (runs_dispatch.go's byoiSelftest(…, false)).
  // Its run.selftest/failure row is not why a run that later failed for its own
  // reason failed, and "refused before any task ran" would be a false diagnosis
  // of a run that ran. Only an explicit false disqualifies a row: the key is
  // absent on run.build and on older trails.
  const cause = firstEvent(
    events,
    (e) => e.outcome === "failure" && e.action in FAILED_CAUSE && e.data?.fail_closed !== false,
  );
  return cause ? from(FAILED_CAUSE[cause.action], cause, cause.action) : { kind: "unknown", action: "" };
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
