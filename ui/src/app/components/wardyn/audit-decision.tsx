/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Who decided a gated call — the run's own policy, or a person.
//
// tool_rules lets a policy answer a tool call without waking anyone
// (internal/egress/proxy/tool_rules.go). An `allow` or a `deny` creates NO
// approval card, so the audit trail is the only place that decision is ever
// visible. If it renders as an ordinary egress row, "policy waved this through"
// is strictly less findable than "someone approved it" — and rules become a
// silent widening.
//
// So a rule-decided row says so, at the same weight as a human decision. The
// only thing that differs on the row is WHO decided and WHICH rule.
//
// Shared because two screens show the same trail: the Audit screen and the run
// page's Audit tab.
import { Link } from "react-router-dom";
import { cn } from "../ui/utils";
import { erasedFieldNames, toolRuleDecision, type AuditEvent } from "../../lib/types";
import { AUDIT } from "../screens/audit-copy";
import { Chip } from "./primitives";
import { PUSH } from "./copy/push";

// toolRuleDecision and RuleDecision moved to lib/types/audit.ts: egressFromAudit
// (lib/api/audit.ts) has to exclude exactly the rows this component relabels,
// and lib must not import from components/. Re-exported so the two mounts — the
// Audit screen and the run page's Audit tab — keep taking the decision and its
// label from one place.
export { toolRuleDecision };
export type { RuleDecision } from "../../lib/types";

// The row's decision label. Renders nothing for an event no rule decided, so a
// caller can fall back to its own description.
//
// The label carries NO size of its own: the two mounts sit on rows at different
// rungs (the Audit screen is text-sm, the run page's Audit tab text-xs), and a
// hard-coded text-sm put two rungs in one row there (§3). Size rides className.
export function AuditDecision({ event, className }: { event: AuditEvent; className?: string }) {
  const decision = toolRuleDecision(event);
  if (!decision) return null;
  return (
    <span className={className}>
      <span className="font-medium text-foreground">Decided by rule</span>{" "}
      <Chip tone="neutral" mono title="rule_source">
        {decision.source}
      </Chip>
    </span>
  );
}

// ruleSourceLabel and RuleSourceLabel moved here from lib/types/audit.ts
// (bundle-split fix, #181): RuleSourceChip below is this function's ONLY
// reader — lib/api/audit.ts's egress projection keys on toolRuleDecision
// alone, never on this — so unlike toolRuleDecision it carries no
// lib-must-not-import-components constraint, and living in the eager
// lib/types/audit.ts module hoisted an always-lazy label table into the
// entry chunk for nothing (same pattern push-content-card.tsx's
// isPushContentRequest documents).
//
// ruleSourceLabel translates a wire rule_source value into console copy.
// Unknown-but-present values fall back to the raw string rather than invented
// copy (CONSOLE-RULES §10: never overclaim); callers pass "" or omit the field
// entirely for "no rule_source" and get null either way.
interface RuleSourceLabel {
  label: string;
  tone: "neutral" | "info" | "danger";
}

// The refusals that name the governing policy (the egress proxy's
// internal/egress/proxy/refusal_attribution.go and the broker refusals it
// attributes). The Request access remedy renders beside a refused row only for
// these: a builtin:* guard or policy:evaluator-error is a fault, and sending
// someone to the policy owner for one costs that owner a ticket that is not theirs.
export const ATTRIBUTED_RULE_SOURCES: ReadonlySet<string> = new Set([
  "policy:denied",
  "policy:default-deny",
  "policy:method",
  "approval:denied",
  "brokered:git",
  "brokered:git-pat:denied",
  "brokered:git:branch-ns",
  "brokered:git:push-rules",
  "brokered:git:push-too-large",
  "brokered:ado:denied",
  "brokered:ado-git:denied",
]);

// True for a refusal row (an egress.deny) whose rule_source is in the set. The
// action check matters: the broker sources also label ALLOW rows.
export function isAttributedRefusal(event: AuditEvent): boolean {
  const source = event.data?.rule_source;
  return event.action === "egress.deny" && typeof source === "string" && ATTRIBUTED_RULE_SOURCES.has(source);
}

export function ruleSourceLabel(source: string): RuleSourceLabel | null {
  if (!source || source.startsWith("policy:tool-")) return null; // toolRuleDecision's rows
  if (source === "policy:allowed") return { label: "Allowed by policy", tone: "neutral" };
  // Every other policy:* value the proxy emits is a refusal (denied,
  // default-deny, method, evaluator-error) — the row's outcome column already
  // says deny; this names WHY at the same weight as the builtin refusals.
  if (source.startsWith("policy:")) return { label: "Refused by policy", tone: "danger" };
  if (source.startsWith("approval:")) return { label: "Released by approval", tone: "neutral" };
  // #181 — a push_rules refusal (never a held request: deny_paths refuses
  // synchronously, before any approval row exists — push_rules.go), named
  // distinctly rather than falling into the generic "Brokered" label every
  // OTHER brokered:* source gets below.
  //
  // DRAFT (canon pending owner approval, packet 7b) — review finding 4: these
  // five labels are the packet-7b proposal, not yet owner-frozen the way the
  // rest of this function's strings are.
  if (source === "brokered:git:push-rules") return { label: "Push refused — a denied path", tone: "danger" };
  if (source === "brokered:git:push-held-unattended") {
    return { label: "Push refused — needs a review nobody can give", tone: "danger" };
  }
  if (source === "brokered:git:push-held") return { label: "Push refused — not approved", tone: "danger" };
  if (source === "brokered:git:push-too-large") return { label: "Push refused — too large to inspect", tone: "danger" };
  if (source === "brokered:git:push-uninspectable") {
    return { label: "Push refused — couldn't be inspected", tone: "danger" };
  }
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
  if (source.startsWith("egress:dropped-decisions-")) return { label: "Decisions dropped", tone: "neutral" };
  return { label: source, tone: "neutral" };
}

// RuleSourceChip is ruleSourceLabel's renderer, for every NON-tool-rule row
// (toolRuleDecision/AuditDecision own those — see the module doc above; a
// tool-rule row's rule_source is never "approval:"/"builtin:"/etc., so the two
// never both render on one event). Mounted beside a row's existing
// host/description text, not instead of it: unlike a tool-rule row, an
// ordinary egress/approval/guard row still names a real target.
//
// approval:<id> is the one source with somewhere to go — there is no
// /approvals/<id> route today, so it links to the Approvals screen's Decided
// tab (an approval that released traffic was, by construction, decided);
// every other source renders the chip bare. The chip's title carries the
// raw wire value so the id, the dropped count or the :branch-ns-off suffix
// the label folds away is still one hover from the row.
export function RuleSourceChip({ event, className }: { event: AuditEvent; className?: string }) {
  const source = event.data?.rule_source;
  if (typeof source !== "string") return null;
  const label = ruleSourceLabel(source);
  if (!label) return null;
  const chip = (
    <Chip tone={label.tone} mono title={source}>
      {label.label}
    </Chip>
  );
  // Cause (0.7.8): a dial-shaped builtin:dial-failed/builtin:gateway-vet-failed
  // row otherwise reads only as "Refused by the built-in guard" — the generic
  // chip a field report traced back an hour to a plain-text 502 body. data.cause
  // is already masked+redacted proxy-side (egress.DecisionLog.Cause), so it is
  // safe to render verbatim beside the chip rather than only on hover.
  const cause = event.data?.cause;
  const causeSpan =
    typeof cause === "string" && cause !== "" ? (
      <span className="min-w-0 truncate text-meta text-muted-foreground" title={cause}>
        {cause}
      </span>
    ) : null;
  const wrapperClassName = cn("inline-flex min-w-0 items-center gap-2", className);
  if (source.startsWith("approval:")) {
    return (
      <span className={wrapperClassName}>
        <Link to="/approvals?tab=decided">{chip}</Link>
        {causeSpan}
      </span>
    );
  }
  // #181 — a push_rules.deny_paths refusal carries no cause text at all (the
  // offending paths ride the sidecar's structured log and the refusal body,
  // never the decision log — push_rules.go's own doc), so the canned
  // explanation substitutes for the causeSpan every OTHER refusal above gets
  // from real data.
  if (source === "brokered:git:push-rules") {
    return (
      <span className={wrapperClassName}>
        {chip}
        <span className="min-w-0 truncate text-meta text-muted-foreground">{PUSH.DENIED_PATH_BODY}</span>
      </span>
    );
  }
  return (
    <span className={wrapperClassName}>
      {chip}
      {causeSpan}
    </span>
  );
}

// A field the person it describes had erased (a sealed field whose key was
// destroyed) renders "Erased", never the wire sentinel (mock packet M4,
// surface D). Nothing else on the row changes; shared by the two mounts that
// show a trail, like AuditDecision above. One note per erased field, named by
// its key, with the explanation as its title. The target is not here: its row
// shows it in place.
export function ErasedFields({ event, className }: { event: AuditEvent; className?: string }) {
  const names = erasedFieldNames(event).filter((n) => n !== "target");
  if (names.length === 0) return null;
  return (
    <>
      {names.map((n) => (
        <span key={n} className={cn("text-muted-foreground", className)} title={AUDIT.ERASED_HINT}>
          {n}: {AUDIT.ERASED}
        </span>
      ))}
    </>
  );
}
