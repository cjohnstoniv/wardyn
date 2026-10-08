/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The authored policy source: one string plus the format it is written in.
// Everything that reads or rewrites it goes through here, so a structured edit
// is always parse → mutate → serialise → the string, and never a parsed
// document kept in state. Loads the source parser: editors only.
import {
  editPolicySource,
  parsePolicySource,
  type PolicySourceEditResult,
  type PolicySourceError,
  type PolicySourceFormat,
  type PolicySourceValue,
} from "../../../lib/policy-document";
import type { RunPolicySpec } from "../../../lib/types";
import { toYaml } from "../code-block";

export type { PolicySourceError, PolicySourceFormat };

// A successful parse proves a JSON-compatible mapping, nothing more: the server
// decides what is a legal policy, and says so with a field path.
export type ParsedSpec = { ok: true; spec: RunPolicySpec } | PolicySourceError;

export function parseSpec(source: string, format: PolicySourceFormat = "yaml"): ParsedSpec {
  const parsed = parsePolicySource(source, format);
  return parsed.ok ? { ok: true, spec: parsed.value as unknown as RunPolicySpec } : parsed;
}

/** A spec written out as source. Generated text carries no comments. */
export function specToSource(spec: RunPolicySpec | Record<string, unknown>, format: PolicySourceFormat = "yaml"): string {
  return format === "json" ? JSON.stringify(spec, null, 2) : `${toYaml(spec)}\n`;
}

/** Sets one top-level key (undefined removes it), keeping every unrelated line and comment. */
export function setSpecKey(
  source: string,
  format: PolicySourceFormat,
  key: string,
  value: unknown,
): PolicySourceEditResult {
  return editPolicySource(source, [key], value as PolicySourceValue | undefined, format);
}

// A structured control hands back the whole spec it would like. Only the keys
// it actually changed are written, one at a time, so the rest of the text —
// comments included — is left exactly as the person wrote it. A refused edit
// returns the refusal and leaves the source alone.
export function applySpecChange(
  source: string,
  format: PolicySourceFormat,
  current: RunPolicySpec,
  next: RunPolicySpec,
): PolicySourceEditResult {
  const before = current as unknown as Record<string, unknown>;
  const after = next as unknown as Record<string, unknown>;
  let text = source;
  for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
    if (JSON.stringify(before[key]) === JSON.stringify(after[key])) continue;
    // Through JSON first: a control may leave an undefined property inside the value.
    const value = after[key] === undefined ? undefined : (JSON.parse(JSON.stringify(after[key])) as unknown);
    const edited = setSpecKey(text, format, key, value);
    if (!edited.ok) return edited;
    text = edited.source;
  }
  return { ok: true, source: text };
}
