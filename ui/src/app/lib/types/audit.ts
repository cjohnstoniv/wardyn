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

// rule_source console labels (6a)
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
  // builtin:upstream-proxy is the ONE builtin:* value that is an ALLOW, not a
  // refusal: recorded once per run, at proxy construction, to audit the
  // deliberate SSRF-guard relaxation for the operator's own configured
  // upstream hop — never a per-request decision. Named BEFORE the generic
  // builtin:* bucket below (which is refusals only), so it cannot fall into
  // it and read as a denial that never happened.
  if (source === "builtin:upstream-proxy") {
    return { label: "Corp upstream proxy in path", tone: "info" };
  }
  // builtin:private-ip is the address-range floor, and it is the one guard an
  // operator reliably misreads: a private endpoint (a VPC endpoint, an internal
  // gateway) refused here looks exactly like a policy or an entitlement gap, so
  // the operator goes to their IAM team about a permission that is fine. Name
  // the cause on the row — the rest of the family stays generic.
  if (source === "builtin:private-ip") {
    return { label: "Refused by a built-in address-range rule, not your policy", tone: "danger" };
  }
  // builtin:resolve-failed is the SAME misreading one step earlier: the proxy
  // never learned an address at all (resolver outage, no such name, no address
  // records). Under the generic builtin label it reads as a guard hit, and the
  // operator widens an SSRF control over a DNS outage — so this one names its
  // cause too, and points at the resolver instead.
  if (source === "builtin:resolve-failed") {
    return { label: "Refused because the name did not resolve, not by policy or the address rule", tone: "danger" };
  }
  // builtin:* is the proxy's own guard family (dial-failed, gateway-vet-failed,
  // …) — every remaining value here is a refusal (builtin:upstream-proxy, the
  // one ALLOW in the family, is handled above and never reaches this line).
  if (source.startsWith("builtin:")) return { label: "Refused by the built-in guard", tone: "danger" };
  // brokered:* is every proxy-side brokered lane (git, mint, approvals,
  // recording, scan-result, llm, sso-token, and git's :branch-ns-off suffix).
  if (source.startsWith("brokered:")) return { label: "Brokered", tone: "neutral" };
  if (source === "site-config:internal-host") return { label: "Declared internal host", tone: "info" };
  if (source.startsWith("egress.decisions.dropped:")) return { label: "Decisions dropped", tone: "neutral" };
  return { label: source, tone: "neutral" };
}

// Run detail supporting shapes (UI-side, projected from audit events)
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
  // B3: the approval an `egress.pending` row raised (the audit row's own
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
  | "killed" // an operator killed it
  | "auto_stop" // the idle reaper stopped it — the policy working, not a fault
  // the dispatch-time model-credential refusal (0.7.6 Finding 3): the declared
  // model-access lane could not carry this run. The SERVER's sentence is the
  // whole explanation — there is no ENDING_COPY row for it — and the console's
  // only addition is the sign-in itself, where a sign-in this viewer can
  // complete would repair it.
  | "credential"
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
   * #532). A provider run's door is keyed by this alone (#543); `mechanism`
   * below is not read when it is set.
   */
  provider?: string;
  /**
   * For `credential` with no `provider`: the DECLARED legacy mechanism of the
   * run that was refused (`data.mechanism` — "bedrock_sso",
   * "anthropic_api_key", …), so a surface offering a repair binds to the
   * failed run's own lane rather than to whatever the viewer's claude-code row
   * says today. Absent on an older trail, which reads as "not a lane this
   * console has a door for".
   */
  mechanism?: string;
}
