/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One-line facts read off a policy spec: the chips beside the editor, the
// Policies table and the New Run rail. Kept apart from policy-panel.tsx so a
// read-only surface (the run page's Policy tab) can use them without loading
// the source parser the editor needs.
import type * as React from "react";
import type { RunPolicySpec } from "../../../lib/types";
import { POLICY_DOCUMENT } from "../copy/policy-document";
import type { Chip } from "../primitives";
import { EFFECT_PAST, splitToolRules } from "../policy-tool-rules";

export type ChipTone = NonNullable<React.ComponentProps<typeof Chip>["tone"]>;

// Compact, honest egress summary. allow_all_egress is always the block-list
// phrasing (never "unrestricted") — see wardyn/copy.ts.
export function egressSummary(spec: RunPolicySpec): { label: string; tone: ChipTone } {
  if (spec.allow_all_egress) {
    return { label: POLICY_DOCUMENT.ALLOW_ALL, tone: "info" };
  }
  const n = spec.allowed_domains?.length ?? 0;
  const denied = spec.denied_domains?.length ?? 0;
  if (n === 0) {
    return { label: denied > 0 ? `No egress, ${denied} denied` : "No egress", tone: "neutral" };
  }
  return {
    label: `${n} domain${n === 1 ? "" : "s"} allowed${denied > 0 ? `, ${denied} denied` : ""}`,
    tone: "info",
  };
}

// Honest lifecycle summary — mirrors the reaper's actual semantics
// (internal/lifecycle/lifecycle.go): auto_stop_after_sec <= 0 or unset means the
// run is exempt from idle auto-stop, not "30 minutes by default".
export function lifecycleSummary(spec: RunPolicySpec): string {
  const s = spec.auto_stop_after_sec;
  if (typeof s === "number" && s > 0) return `Auto-stop: ${Math.max(1, Math.round(s / 60))} min idle`;
  return "Runs until stopped";
}

// What happens to a tool no rule names.
export const toolRulesTail = (effect: string) =>
  `Anything else is ${EFFECT_PAST[effect as keyof typeof EFFECT_PAST] ?? effect}.`;

// The new-run rail's one line. It names the tools: "3 rules" alone would say
// nothing about which calls still stop for a human. Null when the run has no
// rules at all, so a policy written before the field existed grows no empty
// rail section — and so does a malformed one, which the panel's own refusal
// names rather than this rail inventing a summary of nothing.
export function toolRulesSummary(spec: RunPolicySpec): string | null {
  const { named, defaultEffect, explicitDefault } = splitToolRules(spec.tool_rules);
  if (named.length === 0 && !explicitDefault) return null;
  const tail = toolRulesTail(defaultEffect);
  if (named.length === 0) return tail;
  const listed = named.map((r) => `${r.tool} ${EFFECT_PAST[r.effect] ?? r.effect}`).join(", ");
  return `${named.length} rule${named.length === 1 ? "" : "s"} · ${listed}. ${tail}`;
}
