/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The deciders editor's rules as pure functions (internal/types/deciders.go): what a rule may hold, so the
// editor stops every combination the server refuses and a 400 stays the fallback.
import type { DeciderKind, DeciderRule } from "../../../lib/api/deciders";
import type { GovernanceUnit } from "../../../lib/api/governance";
import { goDurationMs } from "../../../lib/go-duration";
import { DECIDERS } from "../../wardyn/copy/deciders";

export const OWN_RULE_KINDS: readonly DeciderKind[] = ["elevation", "image", "file_download"];
export const OWNER_REFUSED_KINDS: readonly DeciderKind[] = ["credential", "push_content", "elevation", "image", "file_download"];
export const MAX_WHO = 8;

const MIN_ELEVATION_MS = 15 * 60_000;
const MAX_ELEVATION_MS = 24 * 3_600_000;
const MAX_RENEW_MS = 168 * 3_600_000;

const isOwn = (k: DeciderKind) => OWN_RULE_KINDS.includes(k);
const has = (r: DeciderRule, ...kinds: DeciderKind[]) => kinds.some((k) => r.kinds.includes(k));

// kind -> the 1-based number of another rule in the draft that names it (the first, when two do).
export function kindsHeldElsewhere(draft: DeciderRule[], index: number): Partial<Record<DeciderKind, number>> {
  const held: Partial<Record<DeciderKind, number>> = {};
  draft.forEach((r, i) => {
    if (i === index) return;
    for (const k of r.kinds) if (held[k] === undefined) held[k] = i + 1;
  });
  return held;
}

// Why a kind's checkbox is off in this rule, or null when it is on offer.
export function kindBlock(
  rule: DeciderRule,
  kind: DeciderKind,
  held: Partial<Record<DeciderKind, number>>,
): { reason: "in_rule"; n: number } | { reason: "not_with" } | { reason: "own_rule" } | null {
  const n = held[kind];
  if (n) return { reason: "in_rule", n };
  if (rule.kinds.some((k) => isOwn(k) && k !== kind)) return { reason: "not_with" };
  if (isOwn(kind) && rule.kinds.some((k) => k !== kind)) return { reason: "own_rule" };
  return null;
}

// Checking an own-rule kind clears the others; checking any other kind leaves the rest.
export function toggleKind(rule: DeciderRule, kind: DeciderKind, on: boolean): DeciderRule {
  if (!on) return { ...rule, kinds: rule.kinds.filter((k) => k !== kind) };
  if (!isOwn(kind)) return rule.kinds.includes(kind) ? rule : { ...rule, kinds: [...rule.kinds, kind] };
  const { max_scope: _scope, second_human: _second, ...rest } = rule;
  return kind === "file_download" ? { ...rest, kinds: [kind], second_human: true } : { ...rest, kinds: [kind] };
}

export function ownerAllowed(rule: DeciderRule): { allowed: true } | { allowed: false; reason: "cred" | "request" | "download" | "second" } {
  if (has(rule, "credential", "push_content")) return { allowed: false, reason: "cred" };
  if (has(rule, "elevation", "image")) return { allowed: false, reason: "request" };
  if (has(rule, "file_download")) return { allowed: false, reason: "download" };
  if (rule.second_human) return { allowed: false, reason: "second" };
  return { allowed: true };
}

export function secondHumanState(rule: DeciderRule): "hidden" | "forced" | "forced_download" | "blocked_by_owner" | "free" {
  if (has(rule, "elevation", "image")) return "hidden";
  if (has(rule, "file_download")) return "forced_download";
  if (has(rule, "credential", "push_content")) return "forced";
  if (rule.who.includes("owner")) return "blocked_by_owner";
  return "free";
}

export function scopeRowShown(rule: DeciderRule): boolean {
  return !rule.kinds.some(isOwn);
}

export function ruleProblems(rule: DeciderRule): Array<"elev_max" | "ask_required" | "renew"> {
  if (!rule.kinds.includes("elevation")) return [];
  const problems: Array<"elev_max" | "ask_required" | "renew"> = [];
  const max = rule.max_elevation ? goDurationMs(rule.max_elevation) : NaN;
  if (!(max >= MIN_ELEVATION_MS && max <= MAX_ELEVATION_MS)) problems.push("elev_max");
  if (!rule.eligible_requesters?.length) problems.push("ask_required");
  if (rule.renew_after) {
    const renew = goDurationMs(rule.renew_after);
    if (!(renew >= 0 && renew <= MAX_RENEW_MS)) problems.push("renew");
  }
  return problems;
}

// The nearest ancestor that locked Deciders, walking parent_id up from the profile's parent.
export function lockedBy(profile: GovernanceUnit, profiles: GovernanceUnit[]): GovernanceUnit | null {
  const seen = new Set<string>([profile.id]);
  let id = profile.parent_id;
  while (id && !seen.has(id)) {
    seen.add(id);
    const up = profiles.find((p) => p.id === id);
    if (!up) return null;
    if (up.locked_knobs?.includes("deciders")) return up;
    id = up.parent_id;
  }
  return null;
}

function whoLabel(entry: string): string {
  const label = entry === "owner" ? DECIDERS.WHO_OWNER : entry === "unit_admins" ? DECIDERS.WHO_ADMINS : entry.startsWith("group:") ? DECIDERS.WHO_GROUP(entry.slice("group:".length)) : null;
  return label === null ? entry : label.charAt(0).toLowerCase() + label.slice(1);
}

// "a", "a or b", "a, b or c".
export function whoPhrase(who: string[]): string {
  const parts = who.map(whoLabel);
  return parts.length < 2 ? parts.join("") : `${parts.slice(0, -1).join(", ")} or ${parts[parts.length - 1]}`;
}
