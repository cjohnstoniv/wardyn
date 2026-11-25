/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The words for the agent's own walls (inner_walls) in the Agent rules section: the shipped Review words (#2822), with
// their first letter capitalised so a chip or a radio label reads as a sentence start.
import { presetChips } from "../../../lib/wall-preset-words";
import type { AgentRules } from "../../../lib/types/agent-rules";
import { NR_REVIEW } from "../../wardyn/copy/new-run/review";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

export const sentenceCase = (s: string): string => s.charAt(0).toUpperCase() + s.slice(1);

/** The modes a rule may require; never "off", which the server refuses (a rule only narrows). */
export const SANDBOX_CHOICES = ["read-only", "workspace-write"] as const;
export const APPROVALS_CHOICES = ["harness", "wardyn"] as const;

export const sandboxLabel = (mode: string): string => sentenceCase(NR_REVIEW.INNER_SANDBOX_MODE(mode));
export const approvalsLabel = (a: string): string => sentenceCase(a === "wardyn" ? NR_REVIEW.INNER_APPROVALS_WARDYN : NR_REVIEW.INNER_APPROVALS_HARNESS);

/** The chips on a rule's row: what the walls narrow, then the agent's own sandbox and approvals. */
export function ruleChips(r: Pick<AgentRules, "spec" | "inner_walls">): string[] {
  const inner = [
    ...(r.inner_walls?.approvals && APPROVALS_CHOICES.includes(r.inner_walls.approvals as "harness") ? [approvalsLabel(r.inner_walls.approvals)] : []),
    ...(r.inner_walls?.sandbox_mode && r.inner_walls.sandbox_mode !== "off" ? [sandboxLabel(r.inner_walls.sandbox_mode)] : []),
  ];
  const walls = presetChips(r.spec).filter((c) => !(inner.length > 0 && c === W.CHIP_OPEN));
  return [...walls, ...inner];
}
