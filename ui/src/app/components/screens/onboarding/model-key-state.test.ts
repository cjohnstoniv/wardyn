/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { modelKeyState, ownKeyApplies } from "./model-key-state";

// Appendix A finding 2 + 2b — the FINAL total truth table (REVIEW-1.md
// rulings R1/R2), walked row for row. Three bands:
//   (a) per_user — credential_source === "per_user"
//   (b) shared_bedrock — NOT per_user, mechanism starts with "bedrock_"
//   (c) other — everything else (api-key/subscription mechanisms, or no row)
// `state: "other"` stands in for shared_expired / not_applicable / absent /
// any unrecognised string under a per_user row; "anything else" (bands b/c)
// samples ≥3 distinct `state` values rather than "any" (REVIEW-1.md NIT).
describe("modelKeyState — the final total truth table", () => {
  it("per_user + live (any hasOwn) -> signed_in, done, no button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      expect(
        modelKeyState({ hasOwn, llmReady: true, modelAccess: { state: "live" }, credentialSource: "per_user" }),
      ).toEqual({ result: "signed_in", done: true, button: false, revealAllowed: false, band: "per_user" });
    }
  });

  it("per_user + expiring (any hasOwn) -> expiring, done, button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      expect(
        modelKeyState({ hasOwn, llmReady: true, modelAccess: { state: "expiring" }, credentialSource: "per_user" }),
      ).toEqual({ result: "expiring", done: true, button: true, revealAllowed: false, band: "per_user" });
    }
  });

  it("per_user + not_configured/expired_signin (any hasOwn) -> not_signed_in, NOT done, button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      for (const state of ["not_configured", "expired_signin"] as const) {
        expect(
          modelKeyState({ hasOwn, llmReady: true, modelAccess: { state }, credentialSource: "per_user" }),
        ).toEqual({ result: "not_signed_in", done: false, button: true, revealAllowed: false, band: "per_user" });
      }
    }
  });

  it("per_user + any other state (any hasOwn) -> unknown, NOT done, no button, no reveal (PER_PERSON_NA_BODY)", () => {
    for (const hasOwn of [true, false]) {
      for (const state of ["shared_expired", "not_applicable", "expired_renewable", undefined]) {
        expect(
          modelKeyState({ hasOwn, llmReady: true, modelAccess: state ? { state } : undefined, credentialSource: "per_user" }),
        ).toEqual({ result: "unknown", done: false, button: false, revealAllowed: false, band: "per_user" });
      }
    }
  });

  // (b) NEW — a shared row whose mechanism is Bedrock: a member's own key
  // can never satisfy it (mechanismSatisfied refuses on provider mismatch),
  // so hasOwn is ignored, the form/reveal never appear, same as per_user.
  it("shared_bedrock + shared_expired (any hasOwn, any credentialSource !== per_user) -> shared_expired, NOT done, no button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      for (const credentialSource of [undefined, "shared"]) {
        for (const mechanism of ["bedrock_sso", "bedrock_bearer", "bedrock_env"]) {
          expect(
            modelKeyState({
              hasOwn,
              llmReady: true,
              modelAccess: { state: "shared_expired" },
              credentialSource,
              mechanism,
            }),
          ).toEqual({ result: "shared_expired", done: false, button: false, revealAllowed: false, band: "shared_bedrock" });
        }
      }
    }
  });

  it("shared_bedrock + anything else -> llmReady ? provided : unknown (ADMIN_NOT_READY_BODY when unknown), no button, no reveal", () => {
    for (const hasOwn of [true, false]) {
      for (const state of ["not_applicable", "not_configured", "live", undefined]) {
        expect(
          modelKeyState({
            hasOwn,
            llmReady: true,
            modelAccess: state ? { state } : undefined,
            credentialSource: undefined,
            mechanism: "bedrock_sso",
          }),
        ).toEqual({ result: "provided", done: true, button: false, revealAllowed: false, band: "shared_bedrock" });

        expect(
          modelKeyState({
            hasOwn,
            llmReady: false,
            modelAccess: state ? { state } : undefined,
            credentialSource: "shared",
            mechanism: "bedrock_bearer",
          }),
        ).toEqual({ result: "unknown", done: false, button: false, revealAllowed: false, band: "shared_bedrock" });
      }
    }
  });

  // (c) "other" — rows 5-7 of the original table, unchanged.
  it("other + hasOwn true (any state, any non-per_user/non-bedrock credentialSource) -> own, done, no button, reveal allowed", () => {
    for (const credentialSource of [undefined, "shared", "api_key"]) {
      for (const mechanism of [undefined, "api_key", ""]) {
        expect(
          modelKeyState({ hasOwn: true, llmReady: false, modelAccess: { state: "not_configured" }, credentialSource, mechanism }),
        ).toEqual({ result: "own", done: true, button: false, revealAllowed: true, band: "other" });
      }
    }
  });

  it("other + shared_expired + hasOwn false -> shared_expired, NOT done, no button, reveal shown", () => {
    expect(
      modelKeyState({ hasOwn: false, llmReady: true, modelAccess: { state: "shared_expired" }, credentialSource: undefined }),
    ).toEqual({ result: "shared_expired", done: false, button: false, revealAllowed: true, band: "other" });
  });

  it("other + anything else + hasOwn false -> llmReady ? provided : unknown, reveal shown", () => {
    for (const state of ["not_applicable", "expired_renewable", "some_future_state"]) {
      expect(
        modelKeyState({ hasOwn: false, llmReady: true, modelAccess: { state }, credentialSource: undefined }),
      ).toEqual({ result: "provided", done: true, button: false, revealAllowed: true, band: "other" });
    }
    expect(
      modelKeyState({ hasOwn: false, llmReady: false, modelAccess: undefined, credentialSource: "shared" }),
    ).toEqual({ result: "unknown", done: false, button: false, revealAllowed: true, band: "other" });
  });
});

describe("ownKeyApplies", () => {
  it("false under per_user regardless of mechanism", () => {
    expect(ownKeyApplies({ credential_source: "per_user", mechanism: "bedrock_sso" })).toBe(false);
    expect(ownKeyApplies({ credential_source: "per_user", mechanism: undefined })).toBe(false);
    expect(ownKeyApplies({ credential_source: "per_user", mechanism: "api_key" })).toBe(false);
  });

  it("false under any bedrock_-prefixed mechanism, per_user or not", () => {
    expect(ownKeyApplies({ credential_source: "shared", mechanism: "bedrock_sso" })).toBe(false);
    expect(ownKeyApplies({ credential_source: undefined, mechanism: "bedrock_bearer" })).toBe(false);
  });

  it("true otherwise, including no row at all", () => {
    expect(ownKeyApplies(undefined)).toBe(true);
    expect(ownKeyApplies({ credential_source: "shared", mechanism: "api_key" })).toBe(true);
    expect(ownKeyApplies({ credential_source: undefined, mechanism: undefined })).toBe(true);
  });
});
