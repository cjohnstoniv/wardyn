/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { StartChoice } from "../../../../lib/schedule";
import { isVMKind } from "../../../../lib/types/machine-kind";
import { giveBackText, textLeft } from "../lenses/harness-lens";
import { withKeys } from "../lenses/lens-keys";
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
    case "seed": {
      // A VM starts no harness and the server refuses a start there, though a template can still name one: the seed
      // leaves the start out and holds its first message, as choosing a VM does (chooseMachineKind). A template that
      // also names a workload is left as written: Autonomous takes the start's first message as its task (shapeCarriers).
      const vmStart = isVMKind(action.intent.machine_kind) && action.intent.startup?.kind === "harness" && !action.intent.workload;
      const bare = vmStart ? withKeys(action.intent, { startup: undefined }) : action.intent;
      // An Autonomous draft has a place for that message: it is the task at once, as a switch to Autonomous makes it.
      const intent = bare.experience === "background" ? giveBackText(bare, textLeft(action.intent, bare)) : bare;
      const heldText = textLeft(action.intent, intent);
      return {
        ...draft, intent, base: intent, origin: action.origin,
        guided: { ...emptyGuided(startFromOf(action.origin)), ...(heldText && { heldText }), ...(vmStart && { startLeftOut: true }) }, doc: undefined, schedule: undefined, revision: draft.revision + 1,
      };
    }
    case "edit": {
      const intent = action.apply(draft.intent);
      // Text held for an agent task is that task's: another shape takes none of it.
      const guided = draft.guided.heldText && intent.shape !== draft.intent.shape ? { ...draft.guided, heldText: undefined } : draft.guided;
      return { ...draft, intent, guided, revision: draft.revision + 1 };
    }
    case "guided":
      return { ...draft, guided: { ...draft.guided, ...action.patch } };
    case "doc":
      return { ...draft, doc: action.doc };
    case "commitDoc":
      return {
        ...draft, intent: action.intent, doc: undefined, revision: draft.revision + 1,
        origin: draft.origin.kind === "scratch" ? { kind: "import" } : draft.origin,
        // A document the person commits holds the figures they chose, so leaving macOS takes none of them back, and
        // its agent text is the run as written, so no held text is given back over it.
        guided: { ...draft.guided, macosFill: undefined, heldText: undefined },
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
