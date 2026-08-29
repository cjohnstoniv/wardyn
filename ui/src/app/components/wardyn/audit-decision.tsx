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
import type { AuditEvent } from "../../lib/types";
import { Chip } from "./primitives";

// The rule_source values the proxy actually emits for a tool-rule decision
// (internal/egress/proxy/local_routes.go's ruleSourceToolAllow/ToolDeny). They
// ride an egress.allow / egress.deny audit event's `data.rule_source`, which is
// what makes those two rows distinguishable from real network egress at all.
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

// The row's decision label. Renders nothing for an event no rule decided, so a
// caller can fall back to its own description.
export function AuditDecision({ event, className }: { event: AuditEvent; className?: string }) {
  const decision = toolRuleDecision(event);
  if (!decision) return null;
  return (
    <span className={className}>
      <span className="text-sm font-medium text-foreground">Decided by rule</span>{" "}
      <Chip tone="neutral" mono title="rule_source">
        {decision.source}
      </Chip>
    </span>
  );
}
