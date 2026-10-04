/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { people } from "./people";

afterEach(() => vi.restoreAllMocks());

// The drawer's "Sign out everywhere" promises a sign-out only. Without sessions_only the route also
// revokes every API token and deletes every SSH key, so the flag is the contract.
describe("people.signOutEverywhere", () => {
  it("sends POST /sessions/revoke with the person and sessions_only", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 204 }));
    await expect(people.signOutEverywhere("sub-ana")).resolves.toBeUndefined();
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/sessions\/revoke$/);
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ sub: "sub-ana", sessions_only: true });
  });
});

const json = (body: unknown, truncated = false) =>
  new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json", ...(truncated ? { "X-Wardyn-Truncated": "true" } : {}) } });

// The token read is paged (limit/offset, X-Wardyn-Truncated), newest first and revoked included, so
// one default page can be all retired rows with the live token behind them.
describe("people.tokens", () => {
  it("reads every page while the server says more remain, so an older live token is not lost", async () => {
    const retired = Array.from({ length: 3 }, (_, i) => ({ id: `r${i}`, name: `retired-${i}`, created_at: "2025-02-01T00:00:00Z", revoked_at: "2025-03-01T00:00:00Z" }));
    const live = { id: "live", name: "still-active", created_at: "2025-01-01T00:00:00Z" };
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(retired, true)).mockResolvedValueOnce(json([live]));
    const got = await people.tokens("sub-ana");
    expect(got.map((t) => t.id)).toEqual(["r0", "r1", "r2", "live"]);
    expect(spy).toHaveBeenCalledTimes(2);
    expect(String(spy.mock.calls[0][0])).toMatch(/\/people\/sub-ana\/tokens\?limit=\d+&offset=0$/);
    expect(String(spy.mock.calls[1][0])).toMatch(/\/people\/sub-ana\/tokens\?limit=\d+&offset=3$/);
  });

  it("stops after one page when the server does not mark it truncated", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(json([{ id: "a", name: "a", created_at: "2025-01-01T00:00:00Z" }]));
    expect(await people.tokens("sub-ana")).toHaveLength(1);
    expect(spy).toHaveBeenCalledTimes(1);
  });
});

// GET /people has no read-one route, so get() asks for the principal as the search and follows the
// cursor until the exact principal turns up.
describe("people.get", () => {
  const row = (principal: string) => ({ principal, issuer_kind: "oidc", pre_created: false, active_sessions: 0, api_tokens: 0, ssh_keys: 0, credentials: 0, active_runs: 0 });

  it("finds the exact principal on a later page of its search", async () => {
    const spy = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(json({ people: [row("sub-ana-1")], next_cursor: "sub-ana-1" }))
      .mockResolvedValueOnce(json({ people: [row("sub-ana")] }));
    expect((await people.get("sub-ana"))?.principal).toBe("sub-ana");
    expect(String(spy.mock.calls[0][0])).toContain("q=sub-ana");
    expect(String(spy.mock.calls[1][0])).toContain("cursor=sub-ana-1");
  });

  it("returns undefined when nobody has that principal", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ people: [row("sub-ana-1")] }));
    expect(await people.get("sub-ana")).toBeUndefined();
  });
});
