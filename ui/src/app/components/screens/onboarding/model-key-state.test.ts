/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { modelKeyState } from "./model-key-state";

// Appendix A finding 2 + 2b — the plan's TOTAL truth table, walked row for
// row. `state: "other"` below stands in for shared_expired / not_applicable /
// absent / any unrecognised string under a per_user row (they all land on
// "unknown" — mechanismSatisfied never grades those against an API key).
describe("modelKeyState — the total truth table", () => {
  it("per_user + live (any hasOwn) -> signed_in, done, no button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      expect(modelKeyState({ hasOwn, llmReady: true, modelAccess: { state: "live" }, credentialSource: "per_user" })).toEqual({
        result: "signed_in",
        done: true,
        button: false,
        revealAllowed: false,
      });
    }
  });

  it("per_user + expiring (any hasOwn) -> expiring, done, button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      expect(
        modelKeyState({ hasOwn, llmReady: true, modelAccess: { state: "expiring" }, credentialSource: "per_user" }),
      ).toEqual({ result: "expiring", done: true, button: true, revealAllowed: false });
    }
  });

  it("per_user + not_configured/expired_signin (any hasOwn) -> not_signed_in, NOT done, button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      for (const state of ["not_configured", "expired_signin"] as const) {
        expect(modelKeyState({ hasOwn, llmReady: true, modelAccess: { state }, credentialSource: "per_user" })).toEqual({
          result: "not_signed_in",
          done: false,
          button: true,
          revealAllowed: false,
        });
      }
    }
  });

  it("per_user + any other state (any hasOwn) -> unknown, NOT done, no button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      for (const state of ["shared_expired", "not_applicable", "expired_renewable", undefined]) {
        expect(
          modelKeyState({ hasOwn, llmReady: true, modelAccess: state ? { state } : undefined, credentialSource: "per_user" }),
        ).toEqual({ result: "unknown", done: false, button: false, revealAllowed: false });
      }
    }
  });

  it("shared/none + hasOwn true (any state) -> own, done, no button, reveal allowed (n/a)", () => {
    for (const credentialSource of [undefined, "shared"]) {
      expect(
        modelKeyState({ hasOwn: true, llmReady: false, modelAccess: { state: "not_configured" }, credentialSource }),
      ).toEqual({ result: "own", done: true, button: false, revealAllowed: true });
    }
  });

  it("shared/none + shared_expired + hasOwn false -> shared_expired, NOT done, no button, reveal shown", () => {
    expect(
      modelKeyState({ hasOwn: false, llmReady: true, modelAccess: { state: "shared_expired" }, credentialSource: undefined }),
    ).toEqual({ result: "shared_expired", done: false, button: false, revealAllowed: true });
  });

  it("shared/none + anything else + hasOwn false -> llmReady ? provided : unknown, reveal shown", () => {
    expect(
      modelKeyState({ hasOwn: false, llmReady: true, modelAccess: { state: "not_applicable" }, credentialSource: undefined }),
    ).toEqual({ result: "provided", done: true, button: false, revealAllowed: true });

    expect(
      modelKeyState({ hasOwn: false, llmReady: false, modelAccess: undefined, credentialSource: "shared" }),
    ).toEqual({ result: "unknown", done: false, button: false, revealAllowed: true });
  });
});
