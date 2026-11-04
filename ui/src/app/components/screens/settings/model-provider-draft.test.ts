/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in block in the editor's change check: addressChanged reads the terms the server's oidcAddress
// digest reads, so a save that would delete everyone's sign-in asks first and one that would not, does not.
import { describe, it, expect } from "vitest";
import type { ModelProvider, ProviderOIDC } from "../../../lib/types/site";
import { addressChanged, draftFrom, oidcChange, providerFrom } from "./model-provider-draft";

const OIDC: ProviderOIDC = {
  issuer: "https://sso.example.com",
  client_id: "wardyn",
  scopes: ["a", "b"],
};
const base = (oidc?: ProviderOIDC): ModelProvider => ({
  id: "corp",
  uid: "u-1",
  name: "Corp",
  kind: "custom_endpoint",
  base_url: "https://llm.example.com",
  oidc,
  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
});
const withOIDC = (patch: Partial<ProviderOIDC>) => base({ ...OIDC, ...patch });

describe("addressChanged: the sign-in block", () => {
  it.each<[string, ModelProvider, boolean]>([
    ["the same block", withOIDC({}), false],
    ["the issuer", withOIDC({ issuer: "https://other.example.com" }), true],
    ["the client id", withOIDC({ client_id: "other" }), true],
    ["a scope added", withOIDC({ scopes: ["a", "b", "c"] }), true],
    ["a scope renamed", withOIDC({ scopes: ["a", "c"] }), true],
    ["the scopes in another order", withOIDC({ scopes: ["b", "a"] }), false],
    ["a duplicate removed", withOIDC({ scopes: ["a"] }), true],
    ["a duplicate added", withOIDC({ scopes: ["a", "b", "b"] }), true],
    ["binding console to none", withOIDC({ identity_binding: "none" }), true],
    ["absent binding read as console", withOIDC({ identity_binding: "console" }), false],
    ["an authorize_url added", withOIDC({ authorize_url: "https://sso.example.com/auth" }), true],
    ["a token_url added", withOIDC({ token_url: "https://sso.example.com/token" }), true],
    ["the client secret name", withOIDC({ client_secret_ref: "corp-secret" }), false],
  ])("%s -> %s", (_, next, changed) => {
    expect(addressChanged(withOIDC({}), next)).toBe(changed);
  });

  it("an order-only change of duplicated scopes is no change, a count change is", () => {
    const dup = withOIDC({ scopes: ["b", "a", "a"] });
    expect(addressChanged(dup, withOIDC({ scopes: ["a", "a", "b"] }))).toBe(false);
    expect(addressChanged(dup, withOIDC({ scopes: ["a", "b"] }))).toBe(true);
  });

  it("a binding-only change from none back to an absent binding is a change", () => {
    expect(addressChanged(withOIDC({ identity_binding: "none" }), withOIDC({}))).toBe(true);
    expect(addressChanged(withOIDC({ identity_binding: "none" }), withOIDC({ identity_binding: "none" }))).toBe(false);
  });

  it("adding or removing the block is a change, in both directions", () => {
    expect(addressChanged(base(), withOIDC({}))).toBe(true);
    expect(addressChanged(withOIDC({}), base())).toBe(true);
    expect(addressChanged(base(), base())).toBe(false);
  });

  it("names the direction of a change", () => {
    expect(oidcChange(base(), withOIDC({}))).toBe("to_sign_in");
    expect(oidcChange(withOIDC({}), base())).toBe("to_paste");
    expect(oidcChange(withOIDC({}), withOIDC({ client_id: "x" }))).toBe("edited");
    expect(oidcChange(withOIDC({}), withOIDC({ client_secret_ref: "s" }))).toBeNull();
  });
});

describe("providerFrom: the sign-in block", () => {
  const stored = withOIDC({
    authorize_url: "https://sso.example.com/auth",
    token_url: "https://sso.example.com/token",
    client_secret_ref: "corp-secret",
    identity_binding: "none",
  });
  const harnesses = [{ id: "claude-code", display: "Claude Code" }];

  it("a save with no edit carries authorize_url and token_url through, and is no change", () => {
    const out = providerFrom(draftFrom(stored, harnesses), stored, []);
    expect(out.oidc).toEqual(stored.oidc);
    expect(addressChanged(stored, out)).toBe(false);
  });

  it("an edit to the issuer keeps the two URLs it does not show", () => {
    const draft = { ...draftFrom(stored, harnesses), oidcIssuer: " https://new.example.com " };
    const out = providerFrom(draft, stored, []);
    expect(out.oidc).toMatchObject({
      issuer: "https://new.example.com",
      authorize_url: "https://sso.example.com/auth",
      token_url: "https://sso.example.com/token",
    });
  });

  it("scopes go as held, a repeat included", () => {
    const draft = { ...draftFrom(stored, harnesses), oidcScopes: ["b", "a", "a"] };
    expect(providerFrom(draft, stored, []).oidc?.scopes).toEqual(["b", "a", "a"]);
  });

  it("binding on is absent, off is none", () => {
    const draft = draftFrom(stored, harnesses);
    expect(draft.oidcBound).toBe(false);
    expect(providerFrom({ ...draft, oidcBound: true }, stored, []).oidc?.identity_binding).toBeUndefined();
    expect(providerFrom(draft, stored, []).oidc?.identity_binding).toBe("none");
  });

  it("Paste a token drops the block; a draft with no stored block is on binding by default", () => {
    expect(providerFrom({ ...draftFrom(stored, harnesses), oidcOn: false }, stored, []).oidc).toBeUndefined();
    expect(draftFrom(base(), harnesses).oidcBound).toBe(true);
    expect(draftFrom(base(), harnesses).oidcOn).toBe(false);
  });
});
