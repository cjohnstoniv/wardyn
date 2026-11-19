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

describe("extra headers: the address rule and the record a save writes", () => {
  const HARNESSES = [{ id: "claude-code", display: "Claude Code" }];
  const withHeaders = (extra_headers?: { name: string; value: string }[]): ModelProvider => ({ ...base(), extra_headers });
  const PK = [
    { name: "X-Portkey-Provider", value: "anthropic" },
    { name: "X-Team", value: "blue" },
  ];

  it.each<[string, ModelProvider, boolean]>([
    ["the same rows", withHeaders(PK), false],
    ["the rows in another order", withHeaders([PK[1], PK[0]]), false],
    ["a row added", withHeaders([...PK, { name: "X-Extra", value: "1" }]), true],
    ["a row removed", withHeaders([PK[0]]), true],
    ["a value changed", withHeaders([PK[0], { name: "X-Team", value: "red" }]), true],
    ["a name changed", withHeaders([PK[0], { name: "X-Squad", value: "blue" }]), true],
    ["a name in another case, which the server stores in canonical case", withHeaders([{ name: "x-portkey-provider", value: "anthropic" }, PK[1]]), false],
  ])("%s -> %s", (_, next, changed) => {
    expect(addressChanged(withHeaders(PK), next)).toBe(changed);
  });

  it("no headers, an empty list and a missing key are the same", () => {
    expect(addressChanged(base(), withHeaders([]))).toBe(false);
    expect(addressChanged(base(), withHeaders(PK))).toBe(true);
    expect(addressChanged(withHeaders(PK), base())).toBe(true);
  });

  it("providerFrom sends the trimmed rows for a custom endpoint, with no placeholder key in any row", () => {
    const draft = {
      ...draftFrom(base(), HARNESSES),
      headers: [
        { name: " x-portkey-provider ", value: " @team ", placeholder: "@<slug>" },
        { name: "x-b", value: "c" },
      ],
    };
    const out = providerFrom(draft, null, []);
    expect(out.extra_headers).toStrictEqual([
      { name: "x-portkey-provider", value: "@team" },
      { name: "x-b", value: "c" },
    ]);
    expect(JSON.stringify(out)).not.toContain("placeholder");
  });

  it("providerFrom sends no extra_headers key with zero rows, even when the stored provider had some", () => {
    const stored = withHeaders(PK);
    const out = providerFrom({ ...draftFrom(stored, HARNESSES), headers: [] }, stored, []);
    expect("extra_headers" in JSON.parse(JSON.stringify(out))).toBe(false);
    expect("extra_headers" in JSON.parse(JSON.stringify(providerFrom(draftFrom(base(), HARNESSES), null, [])))).toBe(false);
  });

  it("providerFrom sends no extra_headers key for any other kind", () => {
    const draft = { ...draftFrom({ ...base(), kind: "anthropic_api_key" }, HARNESSES), headers: [{ name: "x-a", value: "b" }] };
    const out = providerFrom(draft, null, []);
    expect(JSON.parse(JSON.stringify(out))).not.toHaveProperty("extra_headers");
    const stored: ModelProvider = { ...base(), kind: "openai_api_key", extra_headers: PK };
    expect(JSON.parse(JSON.stringify(providerFrom(draftFrom(stored, HARNESSES), stored, [])))).not.toHaveProperty("extra_headers");
  });

  it("a stored extra_headers survives an unrelated edit, and is no address change", () => {
    const stored = withHeaders(PK);
    const draft = { ...draftFrom(stored, HARNESSES), name: "Renamed" };
    const out = providerFrom(draft, stored, []);
    expect(out.name).toBe("Renamed");
    expect(out.extra_headers).toStrictEqual(PK);
    expect(addressChanged(stored, out)).toBe(false);
  });

  it("draftFrom reads the stored names as stored, in canonical case, with no placeholder", () => {
    expect(draftFrom(withHeaders(PK), HARNESSES).headers).toStrictEqual(PK);
    expect(draftFrom(base(), HARNESSES).headers).toStrictEqual([]);
  });
});
