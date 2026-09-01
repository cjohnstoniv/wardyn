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
}

// --- Tool-rule decisions ---------------------------------------------------
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

// --- rule_source console labels (6a) ---------------------------------------
// rule_source has ONE other reader in the console: this function, which covers
// every NON-tool-rule value (a real egress/approval/guard decision, not a
// policy-answered tool call — those are toolRuleDecision's, immediately
// above, and this function returns null for them so the two callers never
// double-label one row). Pure and side-effect-free like toolRuleDecision, for
// the same reason: RuleSourceChip (wardyn/audit-decision.tsx) renders it, and
// lib/ must not import components/.
interface RuleSourceLabel {
  label: string;
  tone: "neutral" | "info" | "danger";
}

// ruleSourceLabel translates a wire rule_source value into console copy.
// Unknown-but-present values fall back to the raw string rather than invented
// copy (CONSOLE-RULES §10: never overclaim); callers pass "" or omit the field
// entirely for "no rule_source" and get null either way.
export function ruleSourceLabel(source: string): RuleSourceLabel | null {
  if (!source || source.startsWith("policy:tool-")) return null; // toolRuleDecision's rows
  if (source === "policy:allowed") return { label: "Allowed by policy", tone: "neutral" };
  // Every other policy:* value the proxy emits is a refusal (denied,
  // default-deny, method, evaluator-error) — the row's outcome column already
  // says deny; this names WHY at the same weight as the builtin refusals.
  if (source.startsWith("policy:")) return { label: "Refused by policy", tone: "danger" };
  if (source.startsWith("approval:")) return { label: "Released by approval", tone: "neutral" };
  // builtin:private-ip is the address-range floor, and it is the one guard an
  // operator reliably misreads: a private endpoint (a VPC endpoint, an internal
  // gateway) refused here looks exactly like a policy or an entitlement gap, so
  // the operator goes to their IAM team about a permission that is fine. Name
  // the cause on the row — the rest of the family stays generic.
  if (source === "builtin:private-ip") {
    return { label: "Refused by a built-in address-range rule, not your policy", tone: "danger" };
  }
  // builtin:* is the proxy's own guard family (dial-failed, upstream-proxy, …)
  // — all refusals.
  if (source.startsWith("builtin:")) return { label: "Refused by the built-in guard", tone: "danger" };
  // brokered:* is every proxy-side brokered lane (git, mint, approvals,
  // recording, scan-result, llm, sso-token, and git's :branch-ns-off suffix).
  if (source.startsWith("brokered:")) return { label: "Brokered", tone: "neutral" };
  if (source === "site-config:internal-host") return { label: "Declared internal host", tone: "info" };
  if (source.startsWith("egress.decisions.dropped:")) return { label: "Decisions dropped", tone: "neutral" };
  return { label: source, tone: "neutral" };
}

// --- Run detail supporting shapes (UI-side, projected from audit events) ---
export interface CredentialGrant {
  id: string;
  scope: string;
  audience: string;
  state: "active" | "expired" | "revoked";
  minted_at?: string;
  expires_at?: string;
  jti?: string;
}

export interface EgressDecision {
  id: string;
  time: string;
  domain: string;
  decision: "allow" | "deny" | "pending";
  bytes?: number;
}

// --- Why a run ended badly (M7(b)) -----------------------------------------
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
  | "killed" // an operator killed it
  | "auto_stop" // the idle reaper stopped it — the policy working, not a fault
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
}
