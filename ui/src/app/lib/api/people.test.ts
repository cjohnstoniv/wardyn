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
