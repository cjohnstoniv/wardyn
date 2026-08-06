/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { health } from "./health";

// Regression for the sign-out HIGH finding: signing out only cleared the local
// admin token, never calling the server logout endpoint, so the OIDC session
// cookie survived and the next auth probe silently re-signed-in. logout()
// must hit /api/v1/auth/logout with the cookie attached (credentials:include),
// so the server can clear the session.
describe("health.logout()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("calls the auth/logout endpoint with credentials included", async () => {
    await health.logout();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/auth/logout");
    expect(init?.credentials).toBe("include");
  });

  it("resolves even when the server returns an error (best-effort logout)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("boom", { status: 500 }));
    await expect(health.logout()).resolves.toBeUndefined();
  });

  it("resolves even when the network throws (best-effort logout)", async () => {
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    await expect(health.logout()).resolves.toBeUndefined();
  });
});

// HIGH fix: GET /site-config echoes the stored `integrations` array, and every
// writer in this codebase (setup-screen.tsx, add-integration-dialog.tsx)
// spreads that GET response straight into a PUT body to patch one field — but
// the server hard-400s any PUT carrying a non-empty `integrations`
// (internal/api/site_config.go's handlePutSiteConfig), so every save broke as
// soon as one integration was stored. Pin the round-trip: whatever GET
// returns, the PUT body must never carry `integrations`.
describe("health — site-config integrations round-trip", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("strips integrations from a GET response spread straight into putSiteConfig", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ scm_hosts: ["github.com"], integrations: [{ id: "anthropic_api_key" }] }), {
        status: 200,
      }),
    );
    const got = await health.getSiteConfig();
    expect(got.integrations).toEqual([{ id: "anthropic_api_key" }]); // sanity: GET really carried it

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({}), { status: 200 }));
    // The exact "patch one field" idiom every real caller uses.
    await health.putSiteConfig({ ...got, scm_hosts: ["github.com", "gitlab.com"] });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [, putInit] = fetchMock.mock.calls[1];
    const body = JSON.parse(putInit!.body as string);
    expect(body).not.toHaveProperty("integrations");
    expect(body.scm_hosts).toEqual(["github.com", "gitlab.com"]);
  });
});

// testProxy's optional url — the escape for a host with no public internet
// (see health.ts's doc comment). No url must keep sending a bare POST (the
// default multi-target check); a url must be the ONLY thing in the body.
describe("health.testProxy(url?)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ state: "reached", detail: "ok" }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("sends no body when called with no url", async () => {
    await health.testProxy();
    const [, init] = fetchMock.mock.calls[0];
    expect(init?.body).toBeUndefined();
  });

  it("sends {url} when called with a custom url, and nothing else", async () => {
    await health.testProxy("https://intranet.example.com");
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init!.body as string)).toEqual({ url: "https://intranet.example.com" });
  });
});
