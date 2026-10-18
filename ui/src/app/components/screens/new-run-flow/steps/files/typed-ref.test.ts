/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { emptyGuided, type RunIntent } from "../../draft/run-intent";
import { isTypedRef, refTextKey, withRefText } from "./typed-ref";

const intent = (ref?: string): RunIntent => ({ workspaces: [{ workspace_id: "w1", repos: [ref ? { repo: "acme/api", ref } : { repo: "acme/api" }] }] });
const guided = (text?: string) => ({ ...emptyGuided(), refText: text ? { [refTextKey("w1", "acme/api")]: text } : {} });

// #2107: Review says a typed ref is not checked until the run starts; a picked or default one says nothing new.
describe("typed refs", () => {
  it("records the typed text, and drops it for a pick or a cleared field", () => {
    const k = refTextKey("w1", "acme/api");
    expect(withRefText({}, k, "  feature/x-typo ")).toEqual({ [k]: "feature/x-typo" });
    expect(withRefText({ [k]: "feature/x-typo" }, k, "")).toEqual({});
    expect(withRefText({ other: "main" }, k, "  ")).toEqual({ other: "main" });
  });

  it("is typed while the intent still carries the very ref that was typed", () => {
    expect(isTypedRef(guided("feature/x-typo"), intent("feature/x-typo"), "w1", "acme/api")).toBe(true);
  });

  it("is not typed for a picked ref, a default ref, or a ref changed since (document edit)", () => {
    expect(isTypedRef(guided(), intent("dev"), "w1", "acme/api")).toBe(false);
    expect(isTypedRef(guided(), intent(), "w1", "acme/api")).toBe(false);
    expect(isTypedRef(guided("feature/x-typo"), intent("main"), "w1", "acme/api")).toBe(false);
    expect(isTypedRef(guided("feature/x-typo"), intent("feature/x-typo"), undefined, "acme/api")).toBe(false);
  });
});
