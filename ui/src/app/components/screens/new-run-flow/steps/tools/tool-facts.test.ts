/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import golden from "../../../../../lib/tool-effect.golden.json";
import type { ToolEffect } from "../../../../../lib/tool-effect";
import { CLAUDE_ATTENDED, CLAUDE_GATED, CLAUDE_UNGATED, CODEX, CUSTOM } from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import type { ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { defaultChoices, reasonText, ruleChoices } from "./tool-facts";

const CASES = golden as { name: string; policy_rules: { tool: string; effect: string }[]; lane_default: ToolEffect; tool: string; effect: ToolEffect }[];
const RANK = { allow: 0, hold: 1, deny: 2 };

// A fact whose lane and rows are a golden case's, and whose harness takes any effect.
const factFor = (c: (typeof CASES)[number]): ToolPolicyFact => ({
  ...CLAUDE_UNGATED,
  lane: c.lane_default === "allow" ? "ungated" : "gated",
  policy_rules: c.policy_rules.map((r) => ({ ...r, source: { kind: "policy" } })),
});

describe("ruleChoices", () => {
  it.each(CASES)("greys what is looser than the policy's effect, as tool-effect.golden.json: $name", (c) => {
    const choices = ruleChoices(factFor(c), "Claude Code", c.tool);
    for (const choice of choices) {
      expect(choice.available, `${c.name}: ${choice.effect}`).toBe(RANK[choice.effect] >= RANK[c.effect]);
      expect(!!choice.reason).toBe(!choice.available);
    }
  });

  it("names the tool and the policy's verb in the reason", () => {
    const fact = { ...CLAUDE_UNGATED, policy_rules: [{ tool: "WebFetch", effect: "deny", source: { kind: "policy" } }] };
    const [allow, hold] = ruleChoices(fact, "Claude Code", "WebFetch");
    expect(allow.reason).toBe("Your policy blocks WebFetch. A run can only be stricter.");
    expect(hold.reason).toBe("Your policy blocks WebFetch. A run can only be stricter.");
  });

  it("lets Codex CLI take rules for MCP tools only, with the server's own sentence for the rest", () => {
    const own = ruleChoices(CODEX, "Codex CLI", "WebFetch");
    expect(own.every((c) => !c.available)).toBe(true);
    expect(own[2].reason).toBe("Wardyn can't block Codex CLI's own tools. Rules for its MCP tools still apply.");
    const mcp = ruleChoices(CODEX, "Codex CLI", "mcp__jira__create_issue");
    expect(mcp.map((c) => [c.effect, c.available])).toEqual([["allow", true], ["hold", false], ["deny", true]]);
    expect(mcp[1].reason).toBe("Codex CLI can't pause a tool call for approval.");
  });

  it("on the attended lane, a tool no rule names takes the policy's \"*\" the server graded against, not the lane's", () => {
    const opts = (refused: Partial<Record<ToolEffect, string>>) =>
      (["allow", "hold", "deny"] as const).map((effect) => ({ effect, available: !refused[effect], ...(refused[effect] ? { reason: refused[effect] } : {}) }));
    // The policy's "*" is allow (left out of policy_rules here); WebFetch asks first.
    const fact: ToolPolicyFact = {
      ...CLAUDE_ATTENDED,
      rule_effects: opts({}),
      policy_rules: [{ tool: "WebFetch", effect: "hold", source: { kind: "policy" } }],
    };
    expect(ruleChoices(fact, "Claude Code", "Bash").map((c) => c.available)).toEqual([true, true, true]);
    expect(ruleChoices(fact, "Claude Code", "WebFetch").map((c) => c.available)).toEqual([false, true, true]);
    // A "*" that asks first: Run it is stricter than the policy allows, for any tool no rule names.
    const held = { ...fact, rule_effects: opts({ allow: "policy_stricter_tool" }) };
    expect(ruleChoices(held, "Claude Code", "Bash").map((c) => c.available)).toEqual([false, true, true]);
  });

  it("a named tool's own policy rule lifts the \"*\" refusal the server graded (DR-0223)", () => {
    const opts = (refused: Partial<Record<ToolEffect, string>>) =>
      (["allow", "hold", "deny"] as const).map((effect) => ({ effect, available: !refused[effect], ...(refused[effect] ? { reason: refused[effect] } : {}) }));
    // The policy's "*" denies (left out of policy_rules on the attended lane); Read is allowed by name.
    const fact: ToolPolicyFact = {
      ...CLAUDE_ATTENDED,
      rule_effects: opts({ allow: "policy_stricter_tool", hold: "policy_stricter_tool" }),
      policy_rules: [{ tool: "Read", effect: "allow", source: { kind: "policy" } }],
    };
    expect(ruleChoices(fact, "Claude Code", "Read").map((c) => c.available)).toEqual([true, true, true]);
    const other = ruleChoices(fact, "Claude Code", "Bash");
    expect(other.map((c) => c.available)).toEqual([false, false, true]);
    expect(other[0].reason).toBe("Your policy blocks Bash. A run can only be stricter.");
  });

  it("offers only what the harness can honour before a tool is named", () => {
    expect(ruleChoices(CODEX, "Codex CLI", "").map((c) => c.available)).toEqual([true, false, true]);
  });

  it("offers nothing for a harness Wardyn sets no rules for", () => {
    const choices = ruleChoices(CUSTOM, "Your own tools", "Bash");
    expect(choices.every((c) => !c.available)).toBe(true);
    expect(choices[0].reason).toBe("Wardyn can't set tool rules for Your own tools.");
  });
});

describe("defaultChoices", () => {
  it("greys Run it on the gated lane with the policy's sentence", () => {
    const [allow, hold, deny] = defaultChoices(CLAUDE_GATED, "Claude Code");
    expect([allow.available, hold.available, deny.available]).toEqual([false, true, true]);
    expect(allow.reason).toBe("Your policy asks first for tools with no rule. A run can only be stricter.");
  });

  it("says Codex CLI cannot hold or block", () => {
    const choices = defaultChoices(CODEX, "Codex CLI");
    expect(choices.map((c) => c.available)).toEqual([true, false, false]);
    expect(choices[1].reason).toBe("Codex CLI can't pause a tool call for approval.");
  });
});


describe("reasonText", () => {
  it("returns nothing for a code this console does not know", () => {
    expect(reasonText("a_new_code")).toBeUndefined();
    expect(reasonText(undefined)).toBeUndefined();
  });
});
