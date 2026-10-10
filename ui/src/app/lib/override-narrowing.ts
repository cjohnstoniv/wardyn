/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The OD-1 narrowing table, as data: per override kind and operation, whether a
// person may add, narrow or remove it. internal/types/override_narrowing.go is
// the source and writes override-narrowing.json (TestOverrideNarrowingGolden);
// the server refuses with the same rows, and a panel enables or disables a
// control from them, never from its own rule. override-narrowing.test.ts pins
// this file's unions to the Go constants.
import table from "./override-narrowing.json";

export type OverrideKind =
  | "agent_host"
  | "agent_host_wildcard"
  | "agent_secret"
  | "tool_rule_restrict"
  | "tool_rule_allow"
  | "ado_capability"
  | "git_pat_scope"
  | "push_rule_deny"
  | "push_rule_review";

export type OverrideOp = "add" | "remove" | "narrow";
export type OverrideDirection = "tighten" | "widen";

/**
 * allowed: always, it only takes reach away. ceiling_clamped: allowed to ask,
 * the ceiling may still drop it. refused: never, from a run request.
 */
export type OverrideRule = "allowed" | "ceiling_clamped" | "refused";

export interface OverrideNarrowing {
  kind: OverrideKind;
  op: OverrideOp;
  direction: OverrideDirection;
  rule: OverrideRule;
}

export const OVERRIDE_NARROWING: readonly OverrideNarrowing[] = table as OverrideNarrowing[];

/** The row for one operation; undefined for a pair the table has no row for. */
export function overrideRule(kind: OverrideKind, op: OverrideOp): OverrideNarrowing | undefined {
  return OVERRIDE_NARROWING.find((r) => r.kind === kind && r.op === op);
}

/** Whether a control for this operation may be offered at all. An unknown pair is not offered. */
export function overrideOffered(kind: OverrideKind, op: OverrideOp): boolean {
  const row = overrideRule(kind, op);
  return row !== undefined && row.rule !== "refused";
}

/** Whether the ceiling may still drop what the person asks for, so the preview shows it struck. */
export function overrideMayBeClamped(kind: OverrideKind, op: OverrideOp): boolean {
  return overrideRule(kind, op)?.rule === "ceiling_clamped";
}

/** The kind an Add host entry belongs to: a wildcard is its own, refused, kind. */
export function hostOverrideKind(host: string): OverrideKind {
  return host.includes("*") ? "agent_host_wildcard" : "agent_host";
}
