/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { StartChoice } from "../../../../lib/schedule";
import { emptyGuided, initialDraft, type DocBuffer, type GuidedFacts, type NewRunDraft, type NewRunOrigin, type RunIntent } from "./run-intent";

export type DraftAction =
  | { type: "seed"; origin: NewRunOrigin; intent: RunIntent }
  | { type: "edit"; apply: (intent: RunIntent) => RunIntent }
  | { type: "guided"; patch: Partial<GuidedFacts> }
  | { type: "doc"; doc: DocBuffer | undefined }
  | { type: "commitDoc"; intent: RunIntent }
  | { type: "launchFields"; patch: Partial<NewRunDraft["launch"]> }
  | { type: "schedule"; schedule: StartChoice | undefined }
  | { type: "reset"; owner: string };

const startFromOf = (origin: NewRunOrigin): GuidedFacts["startFrom"] =>
  origin.kind === "template" || origin.kind === "again" ? origin.kind : "scratch";

export function draftReducer(draft: NewRunDraft, action: DraftAction): NewRunDraft {
  switch (action.type) {
    case "seed":
      return {
        ...draft, intent: action.intent, base: action.intent, origin: action.origin,
        guided: emptyGuided(startFromOf(action.origin)), doc: undefined, schedule: undefined, revision: draft.revision + 1,
      };
    case "edit":
      return { ...draft, intent: action.apply(draft.intent), revision: draft.revision + 1 };
    case "guided":
      return { ...draft, guided: { ...draft.guided, ...action.patch } };
    case "doc":
      return { ...draft, doc: action.doc };
    case "commitDoc":
      return {
        ...draft, intent: action.intent, doc: undefined, revision: draft.revision + 1,
        origin: draft.origin.kind === "scratch" ? { kind: "import" } : draft.origin,
      };
    case "launchFields":
      return { ...draft, launch: { ...draft.launch, ...action.patch } };
    case "schedule":
      return { ...draft, schedule: action.schedule };
    case "reset":
      return initialDraft(action.owner);
  }
}

/** Structural equality over JSON-shaped values; key order does not matter. */
export function sameValue(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a).filter((k) => (a as Record<string, unknown>)[k] !== undefined);
  const kb = Object.keys(b).filter((k) => (b as Record<string, unknown>)[k] !== undefined);
  return ka.length === kb.length &&
    ka.every((k) => sameValue((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k]));
}

/** The unsaved guard's rule. A successful create resets the draft, which clears it. */
export function isDirty(draft: NewRunDraft): boolean {
  return !sameValue(draft.intent, draft.base) || draft.doc !== undefined ||
    draft.launch.title !== "" || draft.launch.description !== "" || draft.schedule !== undefined;
}
