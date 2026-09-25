/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";

import { MODEL_ACCESS_AGENT, modelAccessDoor, providerAttention, resolveDoor } from "./model-access";
import { AGENTS, modelAccessActionLine } from "./workspace-providers-copy";
import { absoluteTime } from "./format";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "./test-fixtures";
import type { SetupHarnessTool, SetupModelAccess, SetupStatus } from "./types";

// The fixtures are REAL server shapes (internal/api/modelaccess.go's
// setupModelAccess + memberModelAccess), not hand-picked field bags: whose
// credential a row grades is the whole question this predicate answers, and a
// fixture that cannot occur would prove a render no daemon produces.

/** An enabled per_user + bedrock_sso claude-code row — the deployment shape
 *  where every person signs in for themselves. */
function perUserRow(id = MODEL_ACCESS_AGENT): SetupHarnessTool {
  return {
    id,
    display: id,
    has_gateway: false,
    has_login: true,
    enabled: true,
    mechanism: "bedrock_sso",
    credential_source: "per_user",
  };
}

/** The `shared` roster row: ONE credential for everybody, the operator's. */
function sharedRow(): SetupHarnessTool {
  return {
    id: MODEL_ACCESS_AGENT,
    display: "claude-code",
    has_gateway: false,
    has_login: true,
    enabled: true,
    mechanism: "bedrock_sso",
    credential_source: "shared",
  };
}

function statusFor(access: SetupModelAccess | undefined, harnesses: SetupHarnessTool[]): SetupStatus {
  return baseStatus({ model_access: access, harnesses });
}

const OPERATOR = { operator: true };
const MEMBER = { operator: false };

describe("modelAccessDoor — perUser is the claude-code row, never another agent's", () => {
  it("an enabled per_user bedrock_sso claude-code row makes the door per-user", () => {
    const door = modelAccessDoor(
      statusFor({ state: "not_configured", action: AGENTS.SIGN_IN_AWS }, [perUserRow()]),
      MEMBER,
    );
    expect(door.perUser).toBe(true);
    expect(door.needsAttention).toBe(true);
    expect(door.actionable).toBe(true);
    expect(door.action).toBe(AGENTS.SIGN_IN_AWS);
  });

  it("a per_user row for a DIFFERENT agent does not", () => {
    // `per_user` requires bedrock_sso but NOT claude-code (agent_providers.go),
    // so a codex per-user row is savable — and model_access grades the
    // claude-code row alone (modelaccess.go's modelAccessAgent).
    const door = modelAccessDoor(statusFor({ state: "not_configured" }, [perUserRow("codex")]), MEMBER);
    expect(door.perUser).toBe(false);
  });

  it("a DISABLED claude-code per_user row does not (the server scopes it to the operator)", () => {
    const off = { ...perUserRow(), enabled: false };
    expect(modelAccessDoor(statusFor({ state: "not_configured" }, [off]), MEMBER).perUser).toBe(false);
  });
});

describe("modelAccessDoor — the shared-dead state is audience-aware (Codex #7)", () => {
  // awsSSOCredentialState returns shared_expired for a missing OR dead shared
  // credential for its ADMIN too — setupModelAccess grades the credential's
  // scope, not the viewer's role — so without the operator arm an admin with an
  // ordinary dead shared credential reads "ask your admin" with no button.
  it("needs attention for everyone, and is actionable only for the operator", () => {
    const status = statusFor(
      { state: "shared_expired", action: "Your admin's model credential expired — ask them to reconnect it" },
      [sharedRow()],
    );
    expect(modelAccessDoor(status, MEMBER)).toMatchObject({ needsAttention: true, actionable: false });
    expect(modelAccessDoor(status, OPERATOR)).toMatchObject({ needsAttention: true, actionable: true });
  });

  it("an admin's shared-row `expiring` is actionable (their own repair path)", () => {
    const status = statusFor(
      { state: "expiring", action: "Sign in again before 2026-09-19T14:03:22Z", deadline: "2026-09-19T14:03:22Z" },
      [sharedRow()],
    );
    const door = modelAccessDoor(status, OPERATOR);
    expect(door.actionable).toBe(true);
    expect(door.perUser).toBe(false);
    expect(door.deadline).toBe("2026-09-19T14:03:22Z");
  });

  it("a pin-contradicted session is expired_signin, and carries the server's two-pair sentence", () => {
    const action =
      "Your stored AWS session is for account 111111111111 / role Old; this row now allows 222222222222 / New — sign in again.";
    const door = modelAccessDoor(statusFor({ state: "expired_signin", action }, [perUserRow()]), MEMBER);
    expect(door.actionable).toBe(true);
    expect(door.action).toBe(action);
  });
});

describe("modelAccessDoor — the states with nothing to say", () => {
  it("`live` needs no attention", () => {
    expect(modelAccessDoor(statusFor({ state: "live" }, [perUserRow()]), MEMBER).needsAttention).toBe(false);
  });

  it("`not_applicable` (a mechanism principal, not a person) needs no attention", () => {
    expect(modelAccessDoor(statusFor({ state: "not_applicable" }, [perUserRow()]), OPERATOR)).toMatchObject({
      needsAttention: false,
      actionable: false,
    });
  });

  it("an absent model_access (a legacy install, or an older daemon) needs no attention", () => {
    expect(modelAccessDoor(statusFor(undefined, []), OPERATOR)).toMatchObject({
      state: "",
      needsAttention: false,
      actionable: false,
    });
  });

  it("a null status (nothing fetched yet) needs no attention", () => {
    expect(modelAccessDoor(null, OPERATOR).needsAttention).toBe(false);
  });
});

describe("modelAccessActionLine — the deadline is localised, never a raw UTC stamp", () => {
  it("re-composes `expiring` through the frozen template with the viewer's own clock", () => {
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
    const action = "Sign in again before 2026-09-19T14:03:22Z";
    expect(modelAccessActionLine({ state: "expiring", action })).toBe(action);
  });

  it("never rewords any other state's action", () => {
    const action = "Your admin's model credential expired — ask them to reconnect it";
    expect(modelAccessActionLine({ state: "shared_expired", action })).toBe(action);
    expect(modelAccessActionLine(undefined)).toBe("");
  });
});

// #544 / #540: the door keyed by provider, and what the strip speaks for.
describe("resolveDoor — which door an entrance opens", () => {
  const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;

  it("with no provider block, a login request is today's door", () => {
    expect(resolveDoor(baseStatus(), { login: "aws" }, "user")).toEqual({ kind: "legacy", login: "aws" });
    expect(resolveDoor(baseStatus(), { login: "anthropic" }, "user")).toEqual({ kind: "legacy", login: "anthropic" });
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

  it("a login request with no provider of its kind stays today's door (the server's refusal says where to go)", () => {
    const status = providerStatus([{ provider: gateway, defaultFor: ["claude-code"] }]);
    expect(resolveDoor(status, { login: "aws" }, "user")).toEqual({ kind: "legacy", login: "aws" });
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

  it("the provider doors are User view only: the Admin view keeps today's door and opens no provider door", () => {
    const status = providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]);
    expect(resolveDoor(status, { login: "aws" }, "admin")).toEqual({ kind: "legacy", login: "aws" });
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
    const expiring = providerStatus([{ provider: bedrock, state: "expiring", deadline: "2030-01-01T00:00:00Z" }]);
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
