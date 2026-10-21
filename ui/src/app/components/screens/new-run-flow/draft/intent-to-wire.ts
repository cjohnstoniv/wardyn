/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one serializer: preflight, policy preview and create all send what this
// returns, so the three bodies differ only by the Name dialog's fields. It is
// driven by the template field registry: no hand-built whitelist, no defaults,
// no client-side class clamp. A field it cannot carry is reported, never dropped.

import type { StartWire } from "../../../../lib/schedule";
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

/**
 * Fields chosen at each launch that the draft may still hold: sent, never saved (Save as skips their
 * `excluded` home). `local_folder` is the folder on the person's computer (M4, #2982).
 */
const LAUNCH_CHOSEN = new Set(["local_folder"]);

/**
 * A serializer over one registry. The overlay's row may be named either way
 * (`inline_policy` today, `policy_overlay` once the server renames it).
 */
export function wireSerializer(requestRows: readonly TemplateFieldRule[], policyRows: readonly TemplateFieldRule[]) {
  const REQUEST = carried(requestRows);
  const POLICY = carried(policyRows);
  const overlayCarried = REQUEST.has("inline_policy") || REQUEST.has(POLICY_CARRIER);

  function policyOverlay(spec: Record<string, unknown>, unsupported: string[]): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const [name, value] of Object.entries(spec)) {
      if (value === undefined) continue;
      if (POLICY.has(name)) out[name] = value;
      else unsupported.push(`inline_policy.${name}`);
    }
    return out;
  }

  /** The Name dialog's fields, and a start later (StartWire), which the registry leaves to each launch. */
  return function serialize(intent: RunIntent, launch?: { title: string; description: string; start?: StartWire }): WireResult {
    const unsupported: string[] = [];
    const body: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(intent)) {
      if (value === undefined) continue;
      const overlay = key === "inline_policy";
      if (overlay ? !overlayCarried : !REQUEST.has(key) && !LAUNCH_CHOSEN.has(key)) {
        unsupported.push(key);
        continue;
      }
      if (overlay && value !== null && typeof value === "object") {
        body[POLICY_CARRIER] = policyOverlay(value as Record<string, unknown>, unsupported);
      } else body[key] = value;
    }
    body.mode_contract = "0.9";
    if (launch) {
      body.title = launch.title;
      if (launch.description) body.description = launch.description;
      if (launch.start) Object.assign(body, launch.start);
    }
    return { body: body as CreateRunWire, unsupported };
  };
}

export const intentToWire = wireSerializer(TEMPLATE_REQUEST_FIELDS, TEMPLATE_POLICY_FIELDS);
