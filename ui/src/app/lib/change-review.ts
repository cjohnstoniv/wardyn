/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure helpers behind the change review: the plain name of a field, the one sentence a verdict reads as, and the
// changed fields that are not the widening. The server's diff is the only input; nothing here judges a change, so
// the same change always reads the same way.
import { FIELD_LABEL } from "../components/screens/run-detail/policy-tab-copy";
import { BOUNDARY_DRY_RUN } from "./boundary-copy";
import { CHANGE_REVIEW } from "./change-review-copy";
import { CHANGES, RUBRIC } from "./governance-copy";
import type { ChangeWidening, GovernanceChange } from "./types";

const LABELS: Record<string, string> = { ...FIELD_LABEL, ...BOUNDARY_DRY_RUN.KNOB };

// What a change is about, as the reviewer reads it. A profile's key is its id, which says nothing, so its
// name is read from the server's own diff (the stored row on an edit or a delete, the proposed one on a
// create), as is a runner trust change's runner. Every other kind's key is already its natural key.
export function changeTarget(c: GovernanceChange): string {
  // A runner's key is its id; the diff carries its name on both sides.
  if (c.target_kind === "governance_unit" || c.target_kind === "runner_trust") {
    for (const side of [c.diff.after, c.diff.before]) {
      const name = (side as { name?: unknown } | undefined)?.name;
      if (typeof name === "string" && name) return name;
    }
  }
  return c.target_key;
}

// The kind in the table's own vocabulary: the kind part of a change's label. One this console predates shows its
// raw name.
export function kindLabel(c: GovernanceChange): string {
  return CHANGES.KIND[c.target_kind] ?? c.target_kind;
}

// The console's name for a field path, or null when it has none (the caller then shows the path alone). The full
// path is tried first, then the path without its first segment, so `ceiling.allowed_domains` reads as
// `allowed_domains`.
export function fieldLabel(path: string): string | null {
  const own = (k: string) => (Object.prototype.hasOwnProperty.call(LABELS, k) ? LABELS[k] : null);
  const stripped = path.includes(".") ? path.slice(path.indexOf(".") + 1) : null;
  return own(path) ?? (stripped === null ? null : own(stripped));
}

// The value at a dotted path of `root`. The server's changedPaths names an array as one leaf and a
// whole-value change "(root)".
export function valueAt(root: unknown, path: string): unknown {
  if (path === "(root)") return root;
  let cur: unknown = root;
  for (const key of path.split(".")) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[key];
  }
  return cur;
}

// A barrier by the name the autonomy rubric gives it ("Wall (CC2)"); a value it has no row for as it came.
function barrierName(v: string): string {
  const rows = RUBRIC.ROWS as Record<string, [string, string] | undefined>;
  return rows[`confinement_${v.toLowerCase()}`]?.[0] ?? v;
}

// A value as a sentence or a report row shows it: a list joined, a barrier by its name, an unset value as "Not set".
export function showValue(path: string, v: unknown): string {
  if (v === null || v === undefined || (Array.isArray(v) && v.length === 0)) return CHANGES.DIFF_UNSET;
  const last = path.slice(path.lastIndexOf(".") + 1);
  if (last === "min_confinement_class" && typeof v === "string") return barrierName(v);
  if (Array.isArray(v)) return v.map((x) => (typeof x === "string" ? x : JSON.stringify(x))).join(", ");
  return typeof v === "string" ? v : JSON.stringify(v);
}

// Whether a changed path is the widening the server named: the widened field is the path or one of its parts.
function isWidened(path: string, widens: ChangeWidening[]): boolean {
  const parts = path.split(".");
  return widens.some((w) => path === w.field || parts.includes(w.field));
}

export interface AlsoChange {
  path: string;
  label: string;
  before: string;
  after: string;
}

// The changed fields that are not a widening, with the label and the value each side held. Empty without a
// review or when every changed field is the widening.
export function alsoChanges(c: GovernanceChange): AlsoChange[] {
  const widens = c.diff.review?.widens ?? [];
  return c.diff.changed
    .filter((path) => !isWidened(path, widens))
    .map((path) => ({
      path,
      label: fieldLabel(path) ?? path,
      before: showValue(path, valueAt(c.diff.before, path)),
      after: showValue(path, valueAt(c.diff.after, path)),
    }));
}

// "a, b and c".
function joinAnd(items: string[]): string {
  return items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

// The one plain sentence for a widening change: the new hosts when the allowed hosts widen, the widened fields'
// names for any other widening, then up to two other changed fields. Null for a change that does not widen.
export function reviewSentence(c: GovernanceChange): string | null {
  const review = c.diff.review;
  if (!review || review.verdict !== "widens") return null;
  const target = changeTarget(c);
  const widens = review.widens ?? [];
  const sentences: string[] = [];
  const hosts = widens.find((w) => w.field === "allowed_domains" && (w.entries?.length ?? 0) > 0);
  if (hosts) sentences.push(CHANGE_REVIEW.SENTENCE_HOSTS(target, hosts.entries!.length));
  const others = widens.filter((w) => w !== hosts).map((w) => fieldLabel(w.field) ?? w.field);
  if (others.length > 0) sentences.push(CHANGE_REVIEW.SENTENCE_WIDENS(target, joinAnd([...new Set(others)])));
  for (const a of alsoChanges(c).slice(0, 2)) sentences.push(CHANGE_REVIEW.SENTENCE_ALSO(a.label, a.before, a.after));
  return sentences.length > 0 ? sentences.join(" ") : null;
}
