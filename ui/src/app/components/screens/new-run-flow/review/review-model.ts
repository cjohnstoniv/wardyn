/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { FIELD_HOME, REVIEW_GROUP_OF_FIELD, type ReviewGroupId } from "../draft/field-homes";
import type { RunIntent } from "../draft/run-intent";

/** A document-only field the draft carries: shown in its group, never editable here. */
export interface DocRow {
  path: string;
  value: string;
}

const show = (v: unknown): string => (typeof v === "string" ? v : JSON.stringify(v));

/** Every present field whose home is the run document, filed under the Review group that shows it. */
export function docOnlyRows(intent: RunIntent): Record<ReviewGroupId, DocRow[]> {
  const rows: Record<ReviewGroupId, DocRow[]> = { run: [], where: [], work: [], tools: [], access: [], limits: [] };
  const add = (path: string, value: unknown) => {
    if (value === undefined || FIELD_HOME[path] !== "document") return;
    rows[REVIEW_GROUP_OF_FIELD[path] ?? "access"].push({ path, value: show(value) });
  };
  for (const [key, value] of Object.entries(intent)) {
    if (key === "inline_policy") {
      for (const [name, v] of Object.entries((value ?? {}) as Record<string, unknown>)) add(`inline_policy.${name}`, v);
    } else add(key, value);
  }
  return rows;
}
