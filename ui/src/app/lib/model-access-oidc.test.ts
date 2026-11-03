/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An endpoint that signs people in is told apart by the server's sign_in on the setup status row, never by its kind:
// the door, the strip, the connection row and the New Run option all read it there.
import { describe, expect, it } from "vitest";
import { isTokenProvider, providerAttention, providerSignsIn, resolveDoor } from "./model-access";
import { connectionRowCopy, connectionRows } from "./model-connections";
import { providerOptionLabel } from "../components/screens/new-run-flow/checks/model-provider-lane";
import { providerStripLine } from "../components/wardyn/model-access-banner";
import { CONNECTIONS } from "../components/wardyn/copy/door";
import { OIDC_SIGN_IN as T } from "../components/wardyn/copy/oidc-signin";
import { MODEL_PROVIDERS, providerStatus } from "./test-fixtures";
import { aheadByHours } from "./test-clock";
import type { SetupModelProvider } from "./types";

const SSO: SetupModelProvider = {
  id: "corp-sso",
  name: "Corp SSO",
  kind: "custom_endpoint",
  harnesses: ["claude-code"],
  host: "llm.contoso.example",
  sign_in: "oidc",
  identity_binding: "console",
};
const UNBOUND: SetupModelProvider = { ...SSO, id: "corp-sso-open", name: "Corp SSO (open)", identity_binding: "none" };
const PASTE = MODEL_PROVIDERS.gateway;

describe("sign_in, not kind", () => {
  it("an endpoint that signs people in is a sign-in provider; one that does not is a token one", () => {
    expect(providerSignsIn(SSO)).toBe(true);
    expect(providerSignsIn(PASTE)).toBe(false);
    expect(providerSignsIn(MODEL_PROVIDERS.bedrock)).toBe(true);
    expect(isTokenProvider(SSO)).toBe(false);
    expect(isTokenProvider(PASTE)).toBe(true);
  });

  it("opens the sign-in door for it, and the typed-token door for the other endpoint", () => {
    const status = providerStatus([{ provider: SSO }, { provider: PASTE }]);
    expect(resolveDoor(status, { provider: "corp-sso" }, "user")).toMatchObject({ kind: "signin", login: "oidc" });
    expect(resolveDoor(status, { provider: PASTE.id }, "user")).toMatchObject({ kind: "key", token: true });
  });

  it("names the New Run option a sign-in", () => {
    const access = providerStatus([{ provider: SSO, state: "live" }]).provider_access;
    expect(providerOptionLabel(SSO, access)).toContain("signed in");
    expect(providerOptionLabel(SSO, providerStatus([{ provider: SSO }]).provider_access)).toContain("not signed in");
  });
});

describe("the strip", () => {
  it("raises for the default of an agent that is not signed in, and for one whose sign-in ended", () => {
    const none = providerStatus([{ provider: SSO, defaultFor: ["claude-code"], state: "not_configured" }]);
    const [a] = providerAttention(none);
    expect(providerStripLine(a, "Claude Code")).toMatchObject({
      sentence: T.STRIP_NONE("Claude Code", "Corp SSO"),
      button: T.BUTTON("Corp SSO"),
      tone: "warning",
      dismissible: false,
    });
    // An ended sign-in speaks even when the provider is no agent's default: the person held one.
    const ended = providerStatus([{ provider: SSO, state: "expired_signin" }]);
    expect(providerStripLine(providerAttention(ended)[0], "")).toMatchObject({
      sentence: T.STRIP_ENDED("Corp SSO"),
      button: T.BUTTON("Corp SSO"),
    });
  });

  it("stays quiet for a signed-in provider", () => {
    expect(providerAttention(providerStatus([{ provider: SSO, defaultFor: ["claude-code"], state: "live" }]))).toEqual([]);
  });
});

describe("the connection row", () => {
  const row = (p: SetupModelProvider, state: string) => {
    const s = providerStatus([{ provider: p, state, addedAt: aheadByHours(-30) }]);
    return connectionRowCopy(s, connectionRows(s)[0]);
  };

  it("not signed in: Sign in, and no Disconnect", () => {
    const s = providerStatus([{ provider: SSO, state: "not_configured" }]);
    const c = connectionRowCopy(s, connectionRows(s)[0]);
    expect(c.chip.label).toBe(CONNECTIONS.NOT_SIGNED_IN);
    expect(c.button).toBe(T.SIGN_IN);
    expect(c.disconnect).toBeUndefined();
    expect(c.line).toBe(CONNECTIONS.SENT_TO("llm.contoso.example"));
  });

  it("signed in: Sign in again and Disconnect", () => {
    const c = row(SSO, "live");
    expect(c.chip).toEqual({ label: CONNECTIONS.SIGNED_IN, tone: "success" });
    expect(c.button).toBe(T.SIGN_IN_AGAIN);
    expect(c.disconnect).toBe(T.DISCONNECT);
    expect(c.unbound).toBeUndefined();
  });

  it("signed in but not bound: the row says so, and what it means", () => {
    expect(row(UNBOUND, "live").unbound).toEqual({ line: T.NOT_BOUND, explain: T.NOT_BOUND_EXPLAIN });
  });

  it("sign-in ended: Signed out, with its sentence", () => {
    const c = row(SSO, "expired_signin");
    expect(c.chip.label).toBe(CONNECTIONS.SIGNED_OUT);
    expect(c.line).toBe(T.STRIP_ENDED("Corp SSO"));
    expect(c.button).toBe(T.SIGN_IN_AGAIN);
    expect(c.disconnect).toBe(T.DISCONNECT);
  });
});
