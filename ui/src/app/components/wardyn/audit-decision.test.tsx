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
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";
import { ruleSourceLabel } from "../../lib/types";
import { AuditDecision, RuleSourceChip, toolRuleDecision } from "./audit-decision";

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

// 6a: ruleSourceLabel/RuleSourceChip cover every NON-tool-rule rule_source —
// toolRuleDecision/AuditDecision above own "policy:tool-allow"/"policy:tool-deny"
// and every "policy:tool-*" value, untouched by this section.
describe("ruleSourceLabel", () => {
  it("labels every enumerated value", () => {
    expect(ruleSourceLabel("policy:allowed")).toEqual({ label: "Allowed by policy", tone: "neutral" });
    expect(ruleSourceLabel("approval:appr_4c8e21")).toEqual({
      label: "Released by approval",
      tone: "neutral",
    });
    expect(ruleSourceLabel("builtin:private-ip")).toEqual({
      label: "Refused by the built-in guard",
      tone: "danger",
    });
    expect(ruleSourceLabel("builtin:dial-failed")).toEqual({
      label: "Refused by the built-in guard",
      tone: "danger",
    });
    expect(ruleSourceLabel("brokered:git")).toEqual({ label: "Brokered", tone: "neutral" });
    expect(ruleSourceLabel("brokered:git:branch-ns-off")).toEqual({ label: "Brokered", tone: "neutral" });
    // every brokered lane the proxy emits, not only git
    for (const s of ["brokered:mint", "brokered:approvals", "brokered:recording", "brokered:scan-result", "brokered:llm", "brokered:sso-token"]) {
      expect(ruleSourceLabel(s), s).toEqual({ label: "Brokered", tone: "neutral" });
    }
    // the proxy's other policy refusals and builtin guards are labelled as such
    for (const s of ["policy:denied", "policy:default-deny", "policy:method", "policy:evaluator-error"]) {
      expect(ruleSourceLabel(s), s).toEqual({ label: "Refused by policy", tone: "danger" });
    }
    expect(ruleSourceLabel("builtin:upstream-proxy")).toEqual({ label: "Refused by the built-in guard", tone: "danger" });
    expect(ruleSourceLabel("site-config:internal-host")).toEqual({
      label: "Declared internal host",
      tone: "info",
    });
    expect(ruleSourceLabel("egress.decisions.dropped:3")).toEqual({
      label: "Decisions dropped",
      tone: "neutral",
    });
  });

  it("falls back to the raw value for an unrecognised source, never invented copy", () => {
    expect(ruleSourceLabel("something-new")).toEqual({ label: "something-new", tone: "neutral" });
  });

  it("defers policy:tool-* to toolRuleDecision — null here, even for a value toolRuleDecision itself would not recognise", () => {
    expect(ruleSourceLabel("policy:tool-allow")).toBeNull();
    expect(ruleSourceLabel("policy:tool-deny")).toBeNull();
    expect(ruleSourceLabel("policy:tool-future-kind")).toBeNull();
  });

  it("negative control: no rule_source at all is null", () => {
    expect(ruleSourceLabel("")).toBeNull();
  });
});

describe("RuleSourceChip", () => {
  function renderChip(over: Partial<AuditEvent> = {}) {
    return render(
      <MemoryRouter>
        <RuleSourceChip event={event(over)} />
      </MemoryRouter>,
    );
  }

  it("renders the label as a mono chip beside the row", () => {
    renderChip({ data: { rule_source: "site-config:internal-host" } });
    expect(screen.getByText("Declared internal host")).toBeInTheDocument();
  });

  it("links an approval source to the Approvals screen (no /approvals/<id> route exists)", () => {
    renderChip({ data: { rule_source: "approval:appr_4c8e21" } });
    const link = screen.getByRole("link");
    expect(link).toHaveAttribute("href", "/approvals?tab=decided");
    expect(link).toHaveTextContent("Released by approval");
    expect(link.getAttribute("href")).toContain("tab=decided");
    // the wire value survives as the chip's title — the id is one hover away
    expect(link.querySelector("[title]")?.getAttribute("title")).toMatch(/^approval:/);
  });

  it("renders bare (no link) for a non-approval source", () => {
    renderChip({ data: { rule_source: "policy:allowed" } });
    expect(screen.queryByRole("link")).toBeNull();
  });

  // Negative control: an event with no rule_source renders no chip at all.
  it("negative control: no rule_source renders nothing", () => {
    const { container } = renderChip();
    expect(container).toBeEmptyDOMElement();
  });

  // Negative control: a tool-rule row's rule_source never reaches a label here
  // (it stays AuditDecision's) — proving the two components partition cleanly.
  it("negative control: a tool-rule source renders nothing (it is AuditDecision's row)", () => {
    const { container } = renderChip({ data: { rule_source: "policy:tool-deny" } });
    expect(container).toBeEmptyDOMElement();
  });
});
