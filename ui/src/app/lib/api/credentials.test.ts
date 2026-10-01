/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { credentials } from "./credentials";
import { aheadByHours } from "../test-clock";

afterEach(() => vi.restoreAllMocks());

const respond = (status: number, body: unknown = null) =>
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(body == null ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }),
  );

// #1477: the inventory of tokens an admin created for someone else, and Revoke.
// Both are routes the daemon already has or gains in the same release; the
// console asks for metadata only and never sends or reads a token value.
describe("credentials.listAdminMintedTokens", () => {
  it("GETs /tokens?minted_for_others=true and returns the rows", async () => {
    const rows = [{ id: "t-1", principal: "p", name: "ci", minted_by: "a", created_at: aheadByHours(-300) }];
    const spy = respond(200, rows);
    await expect(credentials.listAdminMintedTokens()).resolves.toEqual(rows);
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/tokens\?minted_for_others=true$/);
    expect(init?.method).toBe("GET");
  });
  it("drops rows the server should have filtered: self-minted, revoked, or with no minter (an older daemon answers with every token)", async () => {
    const mine = { id: "t-1", principal: "p", name: "ci", minted_by: "a", created_at: aheadByHours(-300) };
    respond(200, [
      mine,
      { ...mine, id: "t-2", minted_by: "" },
      { ...mine, id: "t-3", minted_by: "p" },
      { ...mine, id: "t-4", revoked_at: aheadByHours(-1) },
      { id: "t-5", principal: "p", name: "legacy", created_at: aheadByHours(-300) },
    ]);
    await expect(credentials.listAdminMintedTokens()).resolves.toEqual([mine]);
  });
  it("is an error on a non-2xx, never an empty list", async () => {
    respond(403, { error: "no" });
    await expect(credentials.listAdminMintedTokens()).rejects.toMatchObject({ status: 403 });
  });
});

describe("credentials.revokeToken", () => {
  it("DELETEs /tokens/{id}, encoded", async () => {
    const spy = respond(204);
    await expect(credentials.revokeToken("a/b c")).resolves.toBeUndefined();
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/tokens\/a%2Fb%20c$/);
    expect(init?.method).toBe("DELETE");
  });
  it("throws on a failure, so a token that is still live is never reported revoked", async () => {
    respond(404, { error: "api token not found" });
    await expect(credentials.revokeToken("t-1")).rejects.toMatchObject({ status: 404 });
  });
});
