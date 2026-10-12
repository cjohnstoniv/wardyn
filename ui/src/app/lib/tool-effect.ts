/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy layer's effect for one tool, so a panel can grey a tool the person
// types the same way the server refuses it. The twin of the server's
// policyToolEffect (internal/api/run_harness_tool_rules.go); both are pinned to
// tool-effect.golden.json, which TestToolEffectGolden writes.

export type ToolEffect = "allow" | "hold" | "deny";

export interface ToolRuleLike {
  tool: string;
  effect: ToolEffect | (string & {});
}

/**
 * The tool's exact rule, else the policy's "*", else what the lane does with a
 * tool no rule names (laneDefault). A person's allow for the tool stands only
 * when this is "allow".
 */
export function effectiveToolEffect(policyRules: readonly ToolRuleLike[], laneDefault: ToolEffect, tool: string): ToolEffect {
  const exact = policyRules.find((r) => r.tool === tool);
  if (exact) return exact.effect as ToolEffect;
  const wildcard = policyRules.find((r) => r.tool === "*");
  return (wildcard?.effect as ToolEffect | undefined) ?? laneDefault;
}
