/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one serializer: preflight, policy preview and create all send what this
// returns, so the three bodies differ only by the Name dialog's fields. It is
// driven by the template field registry: no hand-built whitelist, no defaults,
// no client-side class clamp. A field it cannot carry is reported, never dropped.

import { TEMPLATE_POLICY_FIELDS, TEMPLATE_REQUEST_FIELDS, type TemplateFieldRule } from "../../../../lib/template-fields";
import type { RunIntent } from "./run-intent";

/** The request field the intent's policy overlay travels in (`inline_policy` in the document). */
export const POLICY_CARRIER = "policy_overlay";

export type CreateRunWire = Record<string, unknown> & { mode_contract: "0.9" };

export interface WireResult {
  body: CreateRunWire;
  /** Present intent paths the registry does not carry. Each is a Review blocker. */
  unsupported: string[];
}

const carried = (rows: readonly TemplateFieldRule[]) => new Set(rows.filter((r) => !r.excluded && !r.pending).map((r) => r.name));
const REQUEST = carried(TEMPLATE_REQUEST_FIELDS);
const POLICY = carried(TEMPLATE_POLICY_FIELDS);

function policyOverlay(spec: Record<string, unknown>, unsupported: string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(spec)) {
    if (value === undefined) continue;
    if (POLICY.has(name)) out[name] = value;
    else unsupported.push(`inline_policy.${name}`);
  }
  return out;
}

export function intentToWire(intent: RunIntent, launch?: { title: string; description: string }): WireResult {
  const unsupported: string[] = [];
  const body: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(intent)) {
    if (value === undefined) continue;
    if (!REQUEST.has(key)) {
      unsupported.push(key);
      continue;
    }
    if (key === "inline_policy" && value !== null && typeof value === "object") {
      body[POLICY_CARRIER] = policyOverlay(value as Record<string, unknown>, unsupported);
    } else body[key] = value;
  }
  body.mode_contract = "0.9";
  if (launch) {
    body.title = launch.title;
    if (launch.description) body.description = launch.description;
  }
  return { body: body as CreateRunWire, unsupported };
}
