/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Who put a value on Review. The server's provenance rows come first; a field
// they do not cover falls back to the draft: a value the person changed from
// where the origin loaded it is theirs, an unchanged one is the origin's, and
// anything else says nothing rather than guess.

import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SHAPE } from "../../../wardyn/copy/new-run/shape";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import type { PolicyPreviewResult, ProvenanceEntry } from "../../../../lib/types/policy-preview";
import { sameValue } from "../draft/draft-reducer";
import type { NewRunDraft, NewRunOrigin, RunIntent } from "../draft/run-intent";
import type { ReviewPreflight } from "./review-seams";

export interface ProvenanceContext {
  draft: NewRunDraft;
  pre: ReviewPreflight | null;
  preview: PolicyPreviewResult | null;
}

/** A dotted path into the intent. */
export const intentAt = (intent: RunIntent, path: string): unknown =>
  path.split(".").reduce<unknown>((v, key) => (v as Record<string, unknown> | undefined)?.[key], intent);

/** True when the person changed any of the paths from what the origin loaded. */
export const changedFromOrigin = (draft: NewRunDraft, paths: readonly string[]): boolean =>
  paths.some((p) => !sameValue(intentAt(draft.intent, p), intentAt(draft.base, p)));

/** The origin's name for a value it loaded and the person left alone. */
export function originSource(origin: NewRunOrigin): string | undefined {
  switch (origin.kind) {
    case "template": return NR_SHARED.JOIN(NR_SHAPE.SOURCE_TEMPLATE, origin.name);
    case "again": return NR_SHAPE.SOURCE_AGAIN;
    case "import": return NR_REVIEW.DOC_ONLY_CHIP;
    default: return undefined;
  }
}

const named = (label: string, name?: string) => (name ? NR_SHARED.JOIN(label, name) : label);

/** The chip for one server row; a row whose kind has no canon word says nothing. */
function labelOf(row: ProvenanceEntry): string | undefined {
  switch (row.source.kind) {
    case "person": return NR_REVIEW.PROV_ADDED;
    case "policy":
    case "ceiling": return named(NR_REVIEW.PROV_SOURCE_POLICY, row.source.name);
    case "model_provider": return NR_REVIEW.PROV_MODEL_PROVIDER;
    default: return undefined;
  }
}

const rowsOf = (ctx: ProvenanceContext): readonly ProvenanceEntry[] => ctx.pre?.provenance ?? ctx.preview?.provenance ?? [];

/** The server's source for the first row on any of these policy fields (or of this source kind), if it names one. */
export function serverSource(ctx: ProvenanceContext, match: { fields?: readonly string[]; kind?: string }): string | undefined {
  for (const row of rowsOf(ctx)) {
    if (match.fields?.includes(row.field) || row.source.kind === match.kind) {
      const label = labelOf(row);
      if (label) return label;
    }
  }
  return undefined;
}

/** The draft's own account of a value: the person's if they changed it, else the origin's. */
export function draftSource(draft: NewRunDraft, paths: readonly string[], mine: string): string | undefined {
  if (changedFromOrigin(draft, paths)) return mine;
  return originSource(draft.origin);
}

/** Server row first, then the draft. */
export function sourceOf(ctx: ProvenanceContext, match: { fields?: readonly string[]; kind?: string }, paths: readonly string[], mine: string): string | undefined {
  return serverSource(ctx, match) ?? draftSource(ctx.draft, paths, mine);
}
