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
