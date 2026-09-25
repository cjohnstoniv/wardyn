/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit log + the egress projection derived from audit events (the backend has
// no /egress endpoint — egress decisions are read off audit rows).
import type { AuditEvent, EgressDecision, Outcome, RunEnding, RunEndingKind, RunState } from "../types";
// The tool-rule decision lives with the audit shapes it reads (lib/types/audit.ts)
// so both the egress projection below and wardyn/audit-decision.tsx take it from
// one place — lib/api must not import from components/.
import { toolRuleDecision } from "../types";
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
    // A tool call the run's own tool_rules answered is NOT a connection: its
    // target is the control plane, so it rendered in the Egress tile as
    // "Deny · wardynd" — a host the sandbox never dialled — while the Audit
    // tab described the same event as "Decided by rule". One event, two
    // stories. The tile drops exactly the rows that surface relabels.
    .filter((e) => e.action in map && !toolRuleDecision(e))
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
        // B3: only egress.pending stamps one (docs/AUDIT-ACTIONS.md); str()
        // answers undefined for the other two actions and for an older trail.
        approval_id: str(d.approval_id),
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

// B4b — every REQUEST-SCOPED field of a run, read back off its `run.create`
// audit row.
//
// This REPLACES taskModeFromAudit, which read one field of this row the same
// way (its last caller, run-detail.tsx, reads `.task_mode` off this instead —
// there is no wrapper, because one would be a second name for the same read).
// The reason the shape had to widen is the one that reshaped B4b: "everything
// the run record already holds" is not enough to re-run a run. AgentRun
// (lib/types/runs.ts) carries no task_mode, no
// interactive_start, no seed_auto_tools and no tool_approvals — createRunAuditData
// (internal/api/runs.go) stamps all four onto the run.create event precisely
// BECAUSE none of them is stored on the row, which makes that event their only
// durable record. A clone that read the row alone would silently drop a
// tool-approval posture, and "silently" is the whole problem: the second run
// would be less supervised than the one it copied.
//
// Each field is optional on the wire (the Go side omits a zero value), so each
// is optional here. Absent = the server never stamped it: an older trail, or a
// run that did not ask for it. Never guessed.
export type CreateRequestFromAudit = {
  /** "exec" — the run was a plain shell command, no agent harness. */
  task_mode?: string;
  /** What an interactive run's session opened with: "agent" or "shell". */
  interactive_start?: string;
  /** The boot seed was allowed tools before a human attached. */
  seed_auto_tools?: boolean;
  /** "hold" — an autonomous run's tool calls were routed to approvals. */
  tool_approvals?: string;
  /** The policy was written INLINE, and inline policies are never persisted
   *  (internal/api/inline_policy.go attaches with a nil id) — so this is the
   *  one thing a clone knows it cannot carry, and must say so. */
  inline_policy?: boolean;
  /** Every way launch NARROWED the request (resolveRunPolicy's clamp). Read
   *  back so a clone can say what the original was already held to. */
  clamp_warnings?: string[];
};

export function createRequestFromAudit(events: AuditEvent[]): CreateRequestFromAudit {
  const e = events.find((x) => x.action === "run.create");
  const d = (e?.data ?? {}) as Record<string, unknown>;
  const bool = (v: unknown) => (typeof v === "boolean" ? v : undefined);
  return {
    task_mode: str(d.task_mode),
    interactive_start: str(d.interactive_start),
    seed_auto_tools: bool(d.seed_auto_tools),
    tool_approvals: str(d.tool_approvals),
    inline_policy: bool(d.inline_policy),
    clamp_warnings: Array.isArray(d.clamp_warnings)
      ? d.clamp_warnings.filter((w): w is string => typeof w === "string")
      : undefined,
  };
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

// The machine-readable class the dispatch-time model-credential refusal stamps
// on the run.create/failure row it already writes
// (internal/api/runs_dispatch_llm_mechanism.go's llmRefusalAuditReason). It is
// the ONLY thing that distinguishes that refusal from every other run.create
// failure, which is why it is matched exactly and never by sniffing the
// sentence.
const CREDENTIAL_REASON = "model_credential";

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
  if (cause) return from(FAILED_CAUSE[cause.action], cause, cause.action);
  // AFTER the image/selftest scan, deliberately: those are earlier causes, and a
  // credential refusal that followed one of them is fallout.
  const credential = firstEvent(
    events,
    (e) => e.action === "run.create" && e.outcome === "failure" && str(e.data?.reason) === CREDENTIAL_REASON,
  );
  if (credential) {
    return {
      kind: "credential",
      action: credential.action,
      outcome: credential.outcome,
      actor: credential.actor,
      time: credential.time,
      // NO detail: this row's `error` is byte-identical to the run's own
      // failure_hint, which the failure block already renders for an ending
      // with no copy of its own — printing it here would say it twice.
      // A provider row (#532) also writes its kind as `mechanism`, for readers
      // that predate `provider`; this one reads `provider` there instead.
      ...(str(credential.data?.provider)
        ? { provider: str(credential.data?.provider) }
        : { mechanism: str(credential.data?.mechanism) }),
    };
  }
  return { kind: "unknown", action: "" };
}

export const audit = {
  // GET /api/v1/audit?run_id=&action=   (both optional; server-side filter —
  // see parseAuditFilter, internal/api/audit.go). `action` narrows the
  // 1000-row cap to just that action instead of spending the whole budget
  // on every action a chatty run logged: a run-scoped list is
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
