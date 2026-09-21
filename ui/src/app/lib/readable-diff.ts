/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217 — "Copy my changes" hands back the CHANGED FIELDS as text, never the
// whole draft as JSON (issue #217's binding default): the person is about to
// paste this somewhere human, so the one field they edited should not be
// buried under every field they didn't touch.
//
// A plain-object/array walk, not a generic deep-diff library: the console's
// own draft shapes are a handful of top-level fields and short arrays
// (WorkspaceProviders, AgentProviders), so "one line per changed leaf, dotted
// path as the label" is legible without needing a diff renderer.
function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function fmt(v: unknown): string {
  if (v === undefined) return "(none)";
  if (v === null) return "null";
  if (typeof v === "string") return v === "" ? "(empty)" : v;
  if (isPlainObject(v) || Array.isArray(v)) return JSON.stringify(v);
  return String(v);
}

/**
 * Walks two values of the same shape and returns one "path: before → after"
 * line per leaf that changed. Objects recurse key by key; equal-length arrays
 * recurse index by index; anything else that differs (a reordered array, a
 * changed length, a changed primitive) collapses to one line for that whole
 * path — still readable text, never the surrounding untouched fields.
 */
export function readableDiff(before: unknown, after: unknown, path = ""): string[] {
  if (before === after) return [];
  if (isPlainObject(before) && isPlainObject(after)) {
    const keys = new Set([...Object.keys(before), ...Object.keys(after)]);
    return [...keys].flatMap((k) => readableDiff(before[k], after[k], path ? `${path}.${k}` : k));
  }
  if (Array.isArray(before) && Array.isArray(after) && before.length === after.length) {
    return before.flatMap((v, i) => readableDiff(v, after[i], `${path}[${i}]`));
  }
  if (JSON.stringify(before) === JSON.stringify(after)) return [];
  return [`${path || "value"}: ${fmt(before)} → ${fmt(after)}`];
}
