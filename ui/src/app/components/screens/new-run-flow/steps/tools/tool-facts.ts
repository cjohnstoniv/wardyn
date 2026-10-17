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
import type { AccessFacts, EffectOption, ToolPolicyFact } from "../../../../../lib/types/new-run-access";

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

/**
 * The policy's effect for a tool none of `policy_rules` names. On the attended lane the server leaves the
 * policy's "*" out of `policy_rules` (it is no locked row there) but grades `rule_effects` against it, so the
 * least strict effect it does not refuse as stricter than the policy is that "*".
 */
const unnamedToolEffect = (fact: ToolPolicyFact): ToolEffect =>
  fact.lane !== "attended"
    ? laneDefault(fact)
    : EFFECTS.find((e) => fact.rule_effects.find((o) => o.effect === e)?.reason !== "policy_stricter_tool") ?? laneDefault(fact);

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
  const policy = effectiveToolEffect(fact.policy_rules, unnamedToolEffect(fact), tool);
  const builtinOnly = fact.rule_tools === "mcp_only" && !tool.startsWith("mcp__");
  return fromFact(fact.rule_effects).map((o) => {
    const effect = o.effect as ToolEffect;
    // What the harness cannot honour comes first; the policy's strictness is the reason only for what it could.
    const code = builtinOnly ? "harness_no_block_builtin" : !o.available ? o.reason : RANK[effect] < RANK[policy] ? "policy_stricter_tool" : undefined;
    return { effect, available: !code, reason: reasonText(code, { harness: harnessName, tool, policy }) };
  });
}

/** A harness the run does not have names nothing: its row and its tool policy never reach the step. */
export function withoutNamelessHarness(access: AccessFacts): AccessFacts {
  const nameless = (row: AccessFacts["rows"][number]) => row.from === "harness" && !row.name;
  if (!access.rows.some(nameless) && access.harnesses.every((f) => f.harness)) return access;
  return { ...access, rows: access.rows.filter((r) => !nameless(r)), harnesses: access.harnesses.filter((f) => f.harness) };
}
