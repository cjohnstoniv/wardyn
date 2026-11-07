/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicySpec } from "../../lib/types";
import { ToolRulesSection } from "./policy-tool-rules";

const spec = (rules: RunPolicySpec["tool_rules"]): RunPolicySpec => ({ tool_rules: rules } as RunPolicySpec);

describe("ToolRulesSection and approve_each", () => {
  it("offers approve_each between hold and deny on a named rule, and greys it on the default row with its reason", () => {
    render(<ToolRulesSection spec={spec([{ tool: "mcp__jira__delete_issue", effect: "approve_each" }])} onSpecChange={() => {}} />);
    const named = screen.getByRole("combobox", { name: "Effect 1" }) as HTMLSelectElement;
    expect([...named.options].map((o) => o.value)).toEqual(["allow", "hold", "approve_each", "deny"]);
    expect(named.value).toBe("approve_each");
    expect([...named.options].every((o) => !o.disabled)).toBe(true);

    const def = screen.getByRole("combobox", { name: "Default effect" }) as HTMLSelectElement;
    expect([...def.options].find((o) => o.value === "approve_each")?.disabled).toBe(true);
    const note = document.getElementById(def.getAttribute("aria-describedby")!);
    expect(note).toHaveTextContent("Approve each call names one MCP tool. Add it as a rule for that tool.");
  });

  it("writes approve_each to the document, and says it applies on any run", async () => {
    const onChange = vi.fn();
    render(<ToolRulesSection spec={spec([{ tool: "mcp__jira__delete_issue", effect: "hold" }])} onSpecChange={onChange} />);
    expect(screen.getByText(/except approve_each, which the proxy enforces on any run/)).toBeInTheDocument();
    await userEvent.setup().selectOptions(screen.getByRole("combobox", { name: "Effect 1" }), "approve_each");
    expect(onChange.mock.lastCall![0].tool_rules).toEqual([{ tool: "mcp__jira__delete_issue", effect: "approve_each" }]);
  });

  it("shows the shape refusal for approve_each on a tool that is not an MCP tool", () => {
    render(<ToolRulesSection spec={spec([{ tool: "Bash", effect: "approve_each" }])} onSpecChange={() => {}} />);
    expect(screen.getByRole("alert")).toHaveTextContent("cannot be approve_each");
  });

  it.each([
    ["Bash", true],
    ["mcp__jira", true],
    ["mcp__jira__", true],
    ["mcp__jira__delete_issue", false],
  ])("a rule for %s: approve_each greyed is %s, with the reason as text", (tool, greyed) => {
    render(<ToolRulesSection spec={spec([{ tool, effect: "hold" }])} onSpecChange={() => {}} />);
    const select = screen.getByRole("combobox", { name: "Effect 1" }) as HTMLSelectElement;
    expect([...select.options].find((o) => o.value === "approve_each")?.disabled).toBe(greyed);
    if (greyed) {
      expect(document.getElementById(select.getAttribute("aria-describedby")!)).toHaveTextContent("Approve each call names one MCP tool. Add it as a rule for that tool.");
    } else {
      expect(select.getAttribute("aria-describedby")).toBeNull();
    }
  });

  it("a loaded approve_each on a name that cannot take it stays selected, with its refusal, until corrected", () => {
    render(<ToolRulesSection spec={spec([{ tool: "mcp__jira", effect: "approve_each" }])} onSpecChange={() => {}} />);
    expect((screen.getByRole("combobox", { name: "Effect 1" }) as HTMLSelectElement).value).toBe("approve_each");
    expect(screen.getByRole("alert")).toHaveTextContent("cannot be approve_each");
  });
});
