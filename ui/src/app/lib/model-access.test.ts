/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";

import { providerAttention, resolveDoor } from "./model-access";
import { AGENTS, modelAccessActionLine } from "./workspace-providers-copy";
import { absoluteTime } from "./format";
import { aheadByHours } from "./test-clock";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "./test-fixtures";
import type { SetupStatus } from "./types";

describe("modelAccessActionLine — the deadline is localised, never a raw UTC stamp", () => {
  it("re-composes `expiring` through the frozen template with the viewer's own clock", () => {
    // passthrough, never compared to the clock — only re-templated via
    // absoluteTime, which formats absolutely regardless of past/future.
    const deadline = "2026-09-19T14:03:22Z";
    const line = modelAccessActionLine({
      state: "expiring",
      action: `Sign in again before ${deadline}`,
      deadline,
    });
    expect(line).toBe(AGENTS.MODEL_ACCESS_EXPIRING_ACTION(absoluteTime(deadline)));
    expect(line).not.toContain(deadline);
  });

  it("falls back to the server's sentence verbatim with no deadline (an older daemon)", () => {
    // passthrough, never compared to the clock — rendered verbatim.
    const action = "Sign in again before 2026-09-19T14:03:22Z";
    expect(modelAccessActionLine({ state: "expiring", action })).toBe(action);
  });

  it("never rewords any other state's action", () => {
    const action = "Your stored AWS session is for another account — sign in again.";
    expect(modelAccessActionLine({ state: "expired_signin", action })).toBe(action);
    expect(modelAccessActionLine(undefined)).toBe("");
  });
});

// #544 / #540: the door keyed by provider, and what the strip speaks for.
describe("resolveDoor — which door an entrance opens", () => {
  const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;

  it("with no provider block there is no door (#548: the provider door is the only one)", () => {
    expect(resolveDoor(baseStatus(), { login: "aws" }, "user")).toBeNull();
    expect(resolveDoor(baseStatus(), { login: "anthropic" }, "user")).toBeNull();
    expect(resolveDoor(baseStatus(), { provider: "bedrock-prod" }, "user")).toBeNull();
  });

  it("keys a login request to the claude-code default of that sign-in kind, else the first", () => {
    const other = { ...bedrock, id: "bedrock-dev", name: "Bedrock (dev)" };
    const status = providerStatus([
      { provider: other },
      { provider: bedrock, defaultFor: ["claude-code"] },
      { provider: claude },
    ]);
    expect(resolveDoor(status, { login: "aws" }, "user")).toMatchObject({
      kind: "signin",
      login: "aws",
      provider: { id: "bedrock-prod" },
    });
    expect(resolveDoor(status, { login: "anthropic" }, "user")).toMatchObject({
      kind: "signin",
      login: "anthropic",
      provider: { id: "claude-sub" },
    });
    const noDefault = providerStatus([{ provider: other }, { provider: bedrock }]);
    expect(resolveDoor(noDefault, { login: "aws" }, "user")).toMatchObject({ provider: { id: "bedrock-dev" } });
  });

  it("a login request skips a turned-off provider of its kind, default or first", () => {
    const off = { ...bedrock, id: "bedrock-off", name: "Bedrock (off)", disabled: true };
    const first = providerStatus([{ provider: off }, { provider: bedrock }]);
    expect(resolveDoor(first, { login: "aws" }, "user")).toMatchObject({ provider: { id: "bedrock-prod" } });
    const offDefault = providerStatus([{ provider: off, defaultFor: ["claude-code"] }, { provider: bedrock }]);
    expect(resolveDoor(offDefault, { login: "aws" }, "user")).toMatchObject({ provider: { id: "bedrock-prod" } });
  });

  it("a login request with no provider of its kind opens no door", () => {
    const status = providerStatus([{ provider: gateway, defaultFor: ["claude-code"] }]);
    expect(resolveDoor(status, { login: "aws" }, "user")).toBeNull();
  });

  it("a provider request opens that provider's door: a sign-in, or a key/token with whether one is stored", () => {
    const status = providerStatus([{ provider: bedrock }, { provider: gateway, state: "live" }, { provider: anthropicKey }]);
    expect(resolveDoor(status, { provider: "bedrock-prod" }, "user")).toMatchObject({ kind: "signin", login: "aws" });
    expect(resolveDoor(status, { provider: "corp-gateway" }, "user")).toMatchObject({ kind: "key", token: true, stored: true });
    expect(resolveDoor(status, { provider: "anthropic-key" }, "user")).toMatchObject({
      kind: "key",
      token: false,
      stored: false,
    });
    expect(resolveDoor(status, { provider: "nobody" }, "user")).toBeNull();
  });

  it("the provider doors are User view only: the Admin view opens no door", () => {
    const status = providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]);
    expect(resolveDoor(status, { login: "aws" }, "admin")).toBeNull();
    expect(resolveDoor(status, { provider: "bedrock-prod" }, "admin")).toBeNull();
  });
});

describe("providerAttention — what raises the strip (§5.5)", () => {
  const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;
  const ids = (s: SetupStatus) => providerAttention(s).map((a) => a.provider.id);

  it("says nothing with no provider block", () => {
    expect(providerAttention(baseStatus())).toEqual([]);
  });

  it("(i) a default that is not connected; a non-default with nothing held never does (case d)", () => {
    const status = providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }, { provider: anthropicKey }]);
    expect(ids(status)).toEqual(["bedrock-prod"]);
    expect(providerAttention(status)[0].defaultFor).toEqual(["claude-code"]);
  });

  it("(ii) a held AWS sign-in that is dying or dead, default or not (case e)", () => {
    const status = providerStatus([
      { provider: gateway, defaultFor: ["claude-code"], state: "live" },
      { provider: bedrock, state: "expired_signin" },
    ]);
    expect(ids(status)).toEqual(["bedrock-prod"]);
    const expiring = providerStatus([{ provider: bedrock, state: "expiring", deadline: aheadByHours(2) }]);
    expect(ids(expiring)).toEqual(["bedrock-prod"]);
  });

  it("a connected default, a turned-off provider and a Claude sign-in aging are quiet", () => {
    expect(ids(providerStatus([{ provider: gateway, defaultFor: ["codex-cli"], state: "live" }]))).toEqual([]);
    expect(ids(providerStatus([{ provider: { ...bedrock, disabled: true }, defaultFor: ["claude-code"] }]))).toEqual([]);
    expect(ids(providerStatus([{ provider: claude, defaultFor: ["claude-code"], state: "expiring" }]))).toEqual([]);
  });

  it("a default for an agent the person may not run is no default here", () => {
    expect(ids(providerStatus([{ provider: bedrock, defaultFor: ["codex-cli"] }]))).toEqual([]);
  });
});
