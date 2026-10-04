/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit events + the run-detail supporting shapes projected from them.
export type ActorType = "human" | "agent" | "system";

export type Outcome = "success" | "failure" | "denied";

export interface AuditEvent {
  id: string;
  time: string;
  run_id?: string;
  actor_type: ActorType;
  actor: string;
  action: string;
  target?: string;
  outcome: Outcome;
  source_ip?: string;
  data?: Record<string, unknown>;
  // The tamper-evidence chain (internal/types/types.go's AuditEvent.PrevHash/
  // RowHash, migration 0047): row_hash = SHA-256(prev_hash || canonical
  // serialization of the fields above), computed BY POSTGRES on insert.
  // Populated on the WRITE path only — the paginated GET /audit read
  // deliberately does not select them, so both are absent on everything this
  // console renders from that endpoint. The chain itself is verified through
  // GET /api/v1/audit/chain/verify, never by reading these back off a row.
  prev_hash?: string;
  row_hash?: string;
  // The enrolled device that forwarded this row (AuditEvent.DeviceID), read
  // from the stored row; absent on rows this control plane wrote itself.
  device_id?: string;
}

// Tool-rule decisions
// tool_rules lets a policy answer a tool call without waking anyone
// (internal/egress/proxy/tool_rules.go). An `allow` or a `deny` creates NO
// approval card, so the audit trail is the only place that decision is ever
// visible — and it rides an egress.allow / egress.deny event whose target is
// the CONTROL PLANE, because emitLocalDecision logs against controlPlaneURL's
// host (local_routes.go). data.rule_source is what makes it distinguishable
// from real network egress at all.
//
// The one runtime export in this file, and deliberately here: BOTH the egress
// projection that must exclude these rows (lib/api/audit.ts) and the component
// that relabels them (wardyn/audit-decision.tsx) read it, and lib/api must not
// import from components/.
const TOOL_RULE_SOURCES: Record<string, "allow" | "deny"> = {
  "policy:tool-allow": "allow",
  "policy:tool-deny": "deny",
};

export interface RuleDecision {
  /** The effect the rule applied. */
  effect: "allow" | "deny";
  /** The wire rule_source, verbatim — the audit trail's "which rule". */
  source: string;
}

// toolRuleDecision reports whether this event is a tool call the run's own
// tool_rules answered, and which rule did it. Null for everything else —
// including a human's approval.decide, which the row already describes in its
// own words, and real egress, which names a host.
//
// Deliberately keyed on rule_source, not on the action alone: egress.allow is
// also every ordinary allowed connection.
export function toolRuleDecision(e: AuditEvent): RuleDecision | null {
  const source = e.data?.rule_source;
  if (typeof source !== "string") return null;
  const effect = TOOL_RULE_SOURCES[source];
  return effect ? { effect, source } : null;
}

// rule_source console labels (6a): ruleSourceLabel and RuleSourceLabel moved
// to wardyn/audit-decision.tsx (bundle-split fix, #181) — unlike
// toolRuleDecision above, RuleSourceChip (audit-decision.tsx) is its ONLY
// reader; lib/api/audit.ts's egress projection keys on toolRuleDecision alone
// (see audit.test.ts's 6a negative control) and never called this one. So it
// carried no lib-must-not-import-components constraint, and living here only
// hoisted an always-lazy label table (plus #181's five push_* branches) into
// the eager entry chunk for nothing — see push-content-card.tsx's
// isPushContentRequest for the identical pattern.

// Run detail supporting shapes (UI-side, projected from audit events)
export interface CredentialGrant {
  id: string;
  scope: string;
  audience: string;
  state: "active" | "expired" | "revoked";
  minted_at?: string;
  expires_at?: string;
  jti?: string;
  /** The host the grant's scope names (git_pat, ssh_key); absent for other kinds. */
  host?: string;
}

export interface EgressDecision {
  id: string;
  time: string;
  domain: string;
  decision: "allow" | "deny" | "pending";
  bytes?: number;
  // B3: the approval an `egress.hold` row raised (the audit row's own
  // data.approval_id — docs/AUDIT-ACTIONS.md). Absent on allow/deny rows and on
  // an older trail. It exists because a pending ROW is history, not state — the
  // hold it records may have been approved a minute later — so this is the only
  // link from a row the held-count no longer counts to what was decided.
  approval_id?: string;
}

// Why a run ended badly (M7(b))
// The console half of the CLI's runFailureReason (cmd/wardyn/commands.go): the
// reason a run FAILED, was KILLED, or was auto-stopped is only ever in its
// audit trail, and the run page already fetches that trail. No new endpoint.
//
// The set is deliberately small and closed. A cause this build does not
// recognise is "unknown" — the block then renders the state and a link to the
// trail and says nothing more, rather than inventing advice (CONSOLE-RULES §10:
// never overclaim).
export type RunEndingKind =
  | "image" // run.build failed: the sandbox image could not be built, so nothing ran
  | "selftest" // run.selftest failed CLOSED: the wrapped image was refused before any task
  | "killed" // an operator killed it (see RunEnding.evidence for what the trail proves)
  | "auto_stop" // the idle reaper stopped it — the policy working, not a fault
  // the dispatch-time model-credential refusal (0.7.6 Finding 3): the declared
  // model-access lane could not carry this run. The SERVER's sentence is the
  // whole explanation — there is no ENDING_COPY row for it — and the console's
  // only addition is the sign-in itself, where a sign-in this viewer can
  // complete would repair it.
  | "credential"
  // run.create/failure stamped reason "sandbox_create": the runner could not
  // create the sandbox, so nothing ran.
  | "sandbox_create"
  // run.exec/failure stamped reason "agent_start": the sandbox exists but the
  // agent process could not be started in it.
  | "agent_start"
  | "unknown";

export interface RunEnding {
  kind: RunEndingKind;
  /** The audit action carrying the evidence ("run.kill", "run.build", …). */
  action: string;
  outcome?: Outcome;
  /** Who the audit row names — the killer, or wardynd for a system event. */
  actor?: string;
  /** When that event was recorded. */
  time?: string;
  /**
   * The failing step's own reason, verbatim off the audit row — the same
   * error/reason/detail keys, in the same order, the CLI's runFailureReason
   * reads (cmd/wardyn/commands.go). Absent when the row carried none.
   */
  detail?: string;
  /**
   * For `credential`: the model provider the refusal names (`data.provider`,
   * #532). A provider run's door is keyed by this alone (#543).
   */
  provider?: string;
  /**
   * For `killed`: what the trail proves about the kill (#1487). The LATEST
   * run.kill row decides — success is `confirmed`, any other outcome is
   * `partial` (a teardown step failed), and no row at all is `unknown`.
   */
  evidence?: "confirmed" | "partial" | "unknown";
}
