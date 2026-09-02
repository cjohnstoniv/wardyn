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

// /me's 0.7 user-drive pair. The two fields are INDEPENDENT on the wire
// (internal/api/me.go writes them as siblings) because there are four states
// and one field carries three — so the round-trip has to prove both survive,
// including the one a folded field could not express: a shut door with no
// allocation, where "ask an admin for an allocation" is the wrong advice.
describe("health.whoami() — the user-drive pair", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  const meBody = (extra: Record<string, unknown>) => ({
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "member",
    email: "alice@corp.example",
    ...extra,
  });

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const answer = (body: unknown) =>
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );

  it("reads an allocation whole", async () => {
    answer(
      meBody({
        user_drive: {
          name: "Scratch",
          backend: "k8s_pvc",
          size_mib: 16384,
          writable: true,
          enforcement: "request",
          home_name: "d-3f9a1c7e",
        },
        user_drive_denied_by_profile: "",
      }),
    );
    const me = await health.whoami();
    expect(me?.user_drive).toEqual({
      name: "Scratch",
      backend: "k8s_pvc",
      size_mib: 16384,
      writable: true,
      enforcement: "request",
      home_name: "d-3f9a1c7e",
    });
    expect(me?.user_drive_denied_by_profile).toBe("");
  });

  it("reads a paused allocation as an allocation, not as nothing", async () => {
    answer(
      meBody({
        user_drive: {
          name: "Scratch",
          backend: "docker_volume",
          writable: true,
          enforcement: "none",
          paused: true,
        },
        user_drive_denied_by_profile: "",
      }),
    );
    const me = await health.whoami();
    expect(me?.user_drive?.paused).toBe(true);
    // omitempty: no size on the wire is "no allocation shown", never 0 bytes.
    expect(me?.user_drive?.size_mib).toBeUndefined();
  });

  it("carries a shut door with NO allocation — the state a folded field could not express", async () => {
    answer(meBody({ user_drive: null, user_drive_denied_by_profile: "Greenfield contractors" }));
    const me = await health.whoami();
    expect(me?.user_drive).toBeNull();
    expect(me?.user_drive_denied_by_profile).toBe("Greenfield contractors");
  });

  it("leaves both absent on a pre-0.7 daemon — undefined, not a guessed default", async () => {
    answer(meBody({}));
    const me = await health.whoami();
    expect(me?.user_drive).toBeUndefined();
    expect(me?.user_drive_denied_by_profile).toBeUndefined();
  });
});
