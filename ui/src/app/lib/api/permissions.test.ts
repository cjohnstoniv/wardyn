/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The availability routes take the value as the rest of the path, and the
// server compares it byte for byte against the grant rows' value. An image
// ref's ":" sent as "%3A" reaches the handler as "%3A" (Go keeps RawPath when
// the client's escaping differs from its own), so it must go out bare.
import { describe, it, expect, vi, afterEach } from "vitest";
import { permissions } from "./permissions";

afterEach(() => vi.unstubAllGlobals());

describe("permissions availability path", () => {
  it("sends an image ref's slashes, colon and @ bare, and still escapes what a path can't hold", async () => {
    const fetchMock = vi.fn(
      async (_url: RequestInfo | URL) =>
        new Response(JSON.stringify({ kind: "image", value: "x", restricted: false, allowed_by: [] }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await permissions.getAvailability("image", "ghcr.io/acme/dev-toolbox:1.4@sha256:ab");
    await permissions.putAvailability("image", "ghcr.io/acme/a b?c", true);

    expect(String(fetchMock.mock.calls[0][0])).toMatch(
      /\/api\/v1\/permissions\/availability\/image\/ghcr\.io\/acme\/dev-toolbox:1\.4@sha256:ab$/,
    );
    expect(String(fetchMock.mock.calls[1][0])).toMatch(/\/image\/ghcr\.io\/acme\/a%20b%3Fc$/);
  });

  it("escapes !'()* as Go does, so the path is Go's canonical form", async () => {
    const fetchMock = vi.fn(
      async (_url: RequestInfo | URL) =>
        new Response(JSON.stringify({ kind: "image", value: "x", restricted: false, allowed_by: [] }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await permissions.getAvailability("image", "a(b)#c");

    expect(String(fetchMock.mock.calls[0][0])).toMatch(/\/image\/a%28b%29%23c$/);
  });

  // A catalog ref is person-plantable: "../policy/<id>" would otherwise be
  // cleaned by the router into another resource's availability.
  it.each(["../policy/x", "a//b", "."])("refuses %j before any request", async (value) => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    await expect(permissions.getAvailability("image", value)).rejects.toThrow(
      `Invalid availability target: ${JSON.stringify(value)}`,
    );
    await expect(permissions.putAvailability("image", value, true)).rejects.toThrow(/Invalid availability target/);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

// PUT /permissions/enforcement replaces the whole map, so it must carry the
// ETag of the map it was built from: without If-Match a stale page's write
// switches back every kind someone else changed since it loaded.
describe("permissions enforcement ETag", () => {
  it("keeps GET's ETag, sends it as If-Match, and returns the PUT's new ETag", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) =>
      init?.method === "PUT"
        ? new Response(JSON.stringify({ egress_host: true }), { status: 200, headers: { ETag: '"e2"' } })
        : new Response(JSON.stringify({ grants: [], enforcement: { egress_host: false } }), {
            status: 200,
            headers: { ETag: '"e1"' },
          }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const snap = await permissions.getPermissions();
    expect(snap.etag).toBe('"e1"');
    const saved = await permissions.putEnforcement({ egress_host: true }, snap.etag);

    expect(new Headers(fetchMock.mock.calls[1][1]?.headers).get("If-Match")).toBe('"e1"');
    expect(saved).toEqual({ enforcement: { egress_host: true }, etag: '"e2"' });
  });
});
