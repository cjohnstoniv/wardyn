/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpError } from "./core";
import { serviceIdentities } from "./service-identities";

// Each call is reached through fetch stubbed at the global, so the request the console really sends is pinned.
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stub(response: Response) {
  const fetchMock = vi.fn().mockResolvedValue(response);
  vi.stubGlobal("fetch", fetchMock);
  return () => {
    const [url, init] = fetchMock.mock.calls[0];
    return { url: String(url), method: init.method as string, body: init.body as string | undefined };
  };
}

describe("serviceIdentities - request shape", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("listMyInvitations GETs /me/invitations and turns null into []", async () => {
    const sent = stub(json(null));
    expect(await serviceIdentities.listMyInvitations()).toEqual([]);
    expect(sent()).toMatchObject({ url: "/api/v1/me/invitations?limit=1000", method: "GET" });
  });

  it("claimInvitation POSTs the name and ttl, and returns the plaintext once", async () => {
    const sent = stub(json({ id: "t1", token: "wdy_secret", name: "ci" }, 201));
    const got = await serviceIdentities.claimInvitation("i/1", { name: "ci", ttl_seconds: 3600 });
    expect(got.token).toBe("wdy_secret");
    expect(sent()).toEqual({ url: "/api/v1/me/invitations/i%2F1/claim", method: "POST", body: '{"name":"ci","ttl_seconds":3600}' });
  });

  it("listMine GETs /me/service-identities", async () => {
    const sent = stub(json([{ name: "ci-nightly", state: "active" }]));
    expect(await serviceIdentities.listMine()).toHaveLength(1);
    expect(sent()).toMatchObject({ url: "/api/v1/me/service-identities?limit=1000", method: "GET" });
  });

  it("mintToken POSTs to the identity's tokens and returns the plaintext once", async () => {
    const sent = stub(json({ id: "t1", token: "wdy_secret", principal: "service:ci-nightly" }, 201));
    const got = await serviceIdentities.mintToken("ci-nightly", { name: "deploy", ttl_seconds: 86400 });
    expect(got.token).toBe("wdy_secret");
    expect(sent()).toEqual({
      url: "/api/v1/me/service-identities/ci-nightly/tokens",
      method: "POST",
      body: '{"name":"deploy","ttl_seconds":86400}',
    });
  });

  it("listTokens GETs the identity's tokens", async () => {
    const sent = stub(json([{ id: "t1", name: "deploy" }]));
    expect(await serviceIdentities.listTokens("ci-nightly")).toEqual([{ id: "t1", name: "deploy" }]);
    expect(sent()).toMatchObject({ url: "/api/v1/me/service-identities/ci-nightly/tokens?limit=1000", method: "GET" });
  });

  it("revokeToken DELETEs one token and accepts the 204", async () => {
    const sent = stub(new Response(null, { status: 204 }));
    await expect(serviceIdentities.revokeToken("ci-nightly", "t1")).resolves.toBeUndefined();
    expect(sent()).toMatchObject({ url: "/api/v1/me/service-identities/ci-nightly/tokens/t1", method: "DELETE" });
  });

  it("a refusal throws the server's sentence and reason", async () => {
    stub(json({ error: "this service identity is suspended; its tokens cannot be minted", reason: "service_identity_inactive" }, 409));
    const err = await serviceIdentities.mintToken("ci-nightly", { name: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect(err).toMatchObject({ status: 409, reason: "service_identity_inactive" });
    expect((err as HttpError).message).toContain("suspended");
  });
});
