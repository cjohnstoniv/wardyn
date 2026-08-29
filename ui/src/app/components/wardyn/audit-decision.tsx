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
import { ruleSourceLabel, toolRuleDecision, type AuditEvent } from "../../lib/types";
import { Chip } from "./primitives";

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

// RuleSourceChip is ruleSourceLabel's renderer, for every NON-tool-rule row
// (toolRuleDecision/AuditDecision own those — see the module doc above; a
// tool-rule row's rule_source is never "approval:"/"builtin:"/etc., so the two
// never both render on one event). Mounted beside a row's existing
// host/description text, not instead of it: unlike a tool-rule row, an
// ordinary egress/approval/guard row still names a real target.
//
// approval:<id> is the one source with somewhere to go — there is no
// /approvals/<id> route today, so it links to the Approvals screen rather
// than a dead per-id URL; every other source renders the chip bare.
export function RuleSourceChip({ event, className }: { event: AuditEvent; className?: string }) {
  const source = event.data?.rule_source;
  if (typeof source !== "string") return null;
  const label = ruleSourceLabel(source);
  if (!label) return null;
  const chip = (
    <Chip tone={label.tone} mono title="rule_source">
      {label.label}
    </Chip>
  );
  if (source.startsWith("approval:")) {
    return (
      <Link to="/approvals" className={className}>
        {chip}
      </Link>
    );
  }
  return <span className={className}>{chip}</span>;
}
