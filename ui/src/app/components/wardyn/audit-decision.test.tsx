/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A tool_rules `allow` or `deny` creates NO approval card, so audit is the ONLY
// place it is ever visible. These pin the mapping to the rule_source strings the
// proxy really emits (internal/egress/proxy/local_routes.go) — invent one and the
// row silently degrades back into an ordinary egress line, which is exactly the
// invisible widening the surface exists to prevent.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AuditEvent } from "../../lib/types";
import { AuditDecision, toolRuleDecision } from "./audit-decision";

function event(over: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: "evt_1",
    time: "2026-08-28T14:02:11Z",
    actor_type: "agent",
    actor: "run_abc",
    action: "egress.allow",
    outcome: "success",
    ...over,
  };
}

describe("toolRuleDecision", () => {
  it("recognises the proxy's two tool-rule sources", () => {
    expect(toolRuleDecision(event({ data: { rule_source: "policy:tool-allow" } }))).toEqual({
      effect: "allow",
      source: "policy:tool-allow",
    });
    expect(
      toolRuleDecision(event({ action: "egress.deny", data: { rule_source: "policy:tool-deny" } })),
    ).toEqual({ effect: "deny", source: "policy:tool-deny" });
  });

  it("leaves ordinary egress alone — allow is also every allowed connection", () => {
    expect(toolRuleDecision(event({ data: { rule_source: "policy" } }))).toBeNull();
    expect(toolRuleDecision(event({ data: { rule_source: "builtin:private-ip" } }))).toBeNull();
    expect(toolRuleDecision(event())).toBeNull();
  });

  it("is not fooled by a non-string rule_source", () => {
    expect(toolRuleDecision(event({ data: { rule_source: 7 } }))).toBeNull();
  });

  it("never claims a human approval was decided by rule", () => {
    const human = event({
      action: "approval.decide",
      actor_type: "human",
      data: { decision: "APPROVED", approval_id: "appr_4c8e21" },
    });
    expect(toolRuleDecision(human)).toBeNull();
  });
});

describe("AuditDecision", () => {
  it("says who decided and which rule, verbatim", () => {
    render(<AuditDecision event={event({ data: { rule_source: "policy:tool-deny" } })} />);
    expect(screen.getByText("Decided by rule")).toBeInTheDocument();
    expect(screen.getByText("policy:tool-deny")).toBeInTheDocument();
  });

  // The label hard-coded text-sm, so on the run page's text-xs Audit tab the row
  // carried two type rungs at once (§3). The mount owns the size now.
  it("takes its size from the mount, never a rung of its own", () => {
    render(
      <AuditDecision event={event({ data: { rule_source: "policy:tool-deny" } })} className="text-xs" />,
    );
    const label = screen.getByText("Decided by rule");
    expect(label.className).not.toMatch(/\btext-(xs|sm|base|body|meta)\b/);
    expect(label.parentElement).toHaveClass("text-xs");
  });

  it("renders nothing for a row no rule decided, so the caller keeps its own text", () => {
    const { container } = render(<AuditDecision event={event({ data: { rule_source: "policy" } })} />);
    expect(container).toBeEmptyDOMElement();
  });
});
