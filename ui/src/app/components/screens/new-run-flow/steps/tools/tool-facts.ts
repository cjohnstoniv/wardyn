/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a harness's tool policy fact lets the person choose, and why an option is
// greyed. The server's reason codes are turned into its own sentences (the
// golden both sides are pinned to); the greying of a tool the person types uses
// the same twin of the server's policy check as the Go side (tool-effect.ts).

import reasons from "../../../../../lib/access-reasons.golden.json";
import { effectiveToolEffect, type ToolEffect } from "../../../../../lib/tool-effect";
import type { EffectOption, LockedToolRule, ToolPolicyFact } from "../../../../../lib/types/new-run-access";

const SENTENCES = new Map(reasons.map((r) => [r.key, r.text]));
const RANK: Record<ToolEffect, number> = { allow: 0, hold: 1, deny: 2 };
export const EFFECTS: readonly ToolEffect[] = ["allow", "hold", "deny"];

interface ReasonVars {
  harness?: string;
  tool?: string;
  /** The policy's effect for the thing the sentence is about. */
  policy?: ToolEffect;
}

/** The sentence for a closed reason code; undefined for a code this console does not know. */
export function reasonText(code: string | undefined, vars: ReasonVars = {}): string | undefined {
  const raw = code ? SENTENCES.get(code) : undefined;
  if (!raw) return undefined;
  return raw
    .replace("{asks first for|blocks}", vars.policy === "deny" ? "blocks" : "asks first for")
    .replace("{harness}", vars.harness ?? "this harness")
    .replace("{tool}", vars.tool ?? "this tool");
}

/** What the lane does with a tool no rule names: it runs on the ungated lane, otherwise a person is asked. */
const laneDefault = (fact: ToolPolicyFact): ToolEffect => (fact.lane === "ungated" ? "allow" : "hold");

export interface ChoiceState {
  effect: ToolEffect;
  available: boolean;
  /** The sentence that says why it is greyed. */
  reason?: string;
}

const fromFact = (options: readonly EffectOption[]): EffectOption[] =>
  EFFECTS.map((effect) => options.find((o) => o.effect === effect) ?? { effect, available: false });

/** The default's three options as the server offers them, each greyed with its sentence. */
export function defaultChoices(fact: ToolPolicyFact, harnessName: string): ChoiceState[] {
  const policy = effectiveToolEffect(fact.policy_rules, laneDefault(fact), "*");
  return fromFact(fact.default.options).map((o) => ({
    effect: o.effect as ToolEffect,
    available: o.available,
    reason: o.available ? undefined : reasonText(o.reason, { harness: harnessName, policy }),
  }));
}

/**
 * The three effects for one tool the person typed. A rule can only be as strict
 * as the policy's for that tool; beyond that the harness decides what it can
 * honour (Codex rules for its own tools, a custom harness, an attended run).
 */
export function ruleChoices(fact: ToolPolicyFact, harnessName: string, tool: string): ChoiceState[] {
  // Until a tool is named there is nothing of the policy's to compare: only what the harness can honour.
  if (!tool) {
    return fromFact(fact.rule_effects).map((o) => ({ effect: o.effect as ToolEffect, available: o.available, reason: o.available ? undefined : reasonText(o.reason, { harness: harnessName }) }));
  }
  const policy = effectiveToolEffect(fact.policy_rules, laneDefault(fact), tool);
  const builtinOnly = fact.rule_tools === "mcp_only" && !tool.startsWith("mcp__");
  return fromFact(fact.rule_effects).map((o) => {
    const effect = o.effect as ToolEffect;
    // What the harness cannot honour comes first; the policy's strictness is the reason only for what it could.
    const code = builtinOnly ? "harness_no_block_builtin" : !o.available ? o.reason : RANK[effect] < RANK[policy] ? "policy_stricter_tool" : undefined;
    return { effect, available: !code, reason: reasonText(code, { harness: harnessName, tool, policy }) };
  });
}

/** A locked row as the server sends it, plus the `enforced` fact a policy rule may carry (A23). */
export type LockedRow = LockedToolRule & { enforced?: boolean };

/**
 * A locked policy row reads as enforced only when the server names something
 * that enforces it for this harness: Wardyn's gate, or the harness's own
 * settings, or the MCP broker for an MCP tool. `none`, an unknown enforcer and a
 * row the server marks `enforced:false` never do, whatever the lane.
 */
export const lockedRowEnforced = (fact: Pick<ToolPolicyFact, "enforcer">, rule: LockedRow): boolean =>
  rule.enforced !== false &&
  (fact.enforcer === "wardyn_gate" || fact.enforcer === "harness_settings" || (fact.enforcer === "mcp_broker" && rule.tool.startsWith("mcp__")));
