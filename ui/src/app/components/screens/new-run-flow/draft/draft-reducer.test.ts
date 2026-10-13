/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { draftReducer, isDirty, sameValue } from "./draft-reducer";
import { initialDraft, type NewRunDraft, type NewRunOrigin, type RunIntent } from "./run-intent";

const TEMPLATE: NewRunOrigin = { kind: "template", id: "d1111111-1111-4111-8111-111111111111", revision: 2, name: "Nightly", templateKind: "template" };
const LOADED: RunIntent = { shape: "environment", experience: "interactive", image: "registry.example/app:1" };

const seeded = (): NewRunDraft => draftReducer(initialDraft("alice"), { type: "seed", origin: TEMPLATE, intent: LOADED });

describe("draftReducer (U3)", () => {
  it("seed sets the intent and its base, records the origin, clears the editor buffer and bumps the revision", () => {
    const withDoc = draftReducer(initialDraft("alice"), { type: "doc", doc: { format: "yaml", text: "x", diagnostics: [], validated: false } });
    const d = draftReducer(withDoc, { type: "seed", origin: TEMPLATE, intent: LOADED });
    expect(d.intent).toBe(LOADED);
    expect(d.base).toBe(LOADED);
    expect(d.origin).toEqual(TEMPLATE);
    expect(d.guided.startFrom).toBe("template");
    expect(d.doc).toBeUndefined();
    expect(d.revision).toBe(1);
    expect(isDirty(d)).toBe(false);
  });

  it("edit applies a lens change and bumps the revision each time", () => {
    const d1 = draftReducer(seeded(), { type: "edit", apply: (i) => ({ ...i, runner_pool_id: "p" }) });
    const d2 = draftReducer(d1, { type: "edit", apply: (i) => i });
    expect(d1.intent.runner_pool_id).toBe("p");
    expect(d1.base).toBe(LOADED);
    expect([d1.revision, d2.revision]).toEqual([2, 3]);
  });

  it("dirty: an intent that differs from its base, an editor buffer, or typed launch fields", () => {
    expect(isDirty(initialDraft("alice"))).toBe(false);
    expect(isDirty(draftReducer(seeded(), { type: "edit", apply: (i) => ({ ...i, no_end: false }) }))).toBe(true);
    expect(isDirty(draftReducer(seeded(), { type: "edit", apply: (i) => ({ image: i.image, experience: i.experience, shape: i.shape }) }))).toBe(false);
    expect(isDirty(draftReducer(seeded(), { type: "doc", doc: { format: "json", text: "{}", diagnostics: [], validated: true } }))).toBe(true);
    expect(isDirty(draftReducer(seeded(), { type: "launchFields", patch: { title: "Nightly" } }))).toBe(true);
    expect(isDirty(draftReducer(seeded(), { type: "launchFields", patch: { description: "why" } }))).toBe(true);
    expect(isDirty(draftReducer(seeded(), { type: "guided", patch: { mainDriveText: "20" } }))).toBe(false);
  });

  it("commitDoc takes the validated document as the intent and makes a scratch draft an import", () => {
    const d = draftReducer(initialDraft("alice"), { type: "commitDoc", intent: LOADED });
    expect(d.intent).toBe(LOADED);
    expect(d.origin).toEqual({ kind: "import" });
    expect(draftReducer(seeded(), { type: "commitDoc", intent: {} }).origin).toEqual(TEMPLATE);
  });

  it("reset empties everything for the new owner, the Name dialog's fields included", () => {
    let d = draftReducer(seeded(), { type: "launchFields", patch: { title: "Nightly", description: "why" } });
    d = draftReducer(d, { type: "guided", patch: { mainDriveText: "20", refText: { "w/r": "main" } } });
    d = draftReducer(d, { type: "doc", doc: { format: "yaml", text: "x", diagnostics: [], validated: false } });
    const reset = draftReducer(d, { type: "reset", owner: "bob" });
    expect(reset).toEqual(initialDraft("bob"));
    expect(reset.launch).toEqual({ title: "", description: "" });
    expect(isDirty(reset)).toBe(false);
  });

  it("compares values structurally, ignoring key order and undefined keys", () => {
    expect(sameValue({ a: [1, { b: 2 }], c: undefined }, { a: [1, { b: 2 }] })).toBe(true);
    expect(sameValue({ a: [1] }, { a: [1, 2] })).toBe(false);
    expect(sameValue({ a: [] }, { a: {} })).toBe(false);
    expect(sameValue({ a: false }, {})).toBe(false);
  });
});
