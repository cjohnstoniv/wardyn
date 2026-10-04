/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { credentials, ErasureIncomplete } from "./credentials";
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

// POST /people/{principal}/erasure (0.8.6, security tier): the by-scope erase.
describe("credentials.erasePerson", () => {
  it("POSTs the scopes to /people/{principal}/erasure, the principal encoded", async () => {
    const result = { person: "sub-ana", scopes: ["credentials"], outcome: { credentials: "done" } };
    const spy = respond(200, result);
    await expect(credentials.erasePerson("ana@example.com", ["credentials"])).resolves.toEqual(result);
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/people\/ana%40example\.com\/erasure$/);
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ scopes: ["credentials"] });
  });

  it("a partial run is an ErasureIncomplete naming what finished and what is left", async () => {
    respond(500, {
      error: "erasure is not complete; retry with the same scopes to finish the rest",
      reason: "erasure_incomplete",
      done: ["mask_copies"],
      remaining: ["run_outputs", "credentials"],
    });
    const err = await credentials.erasePerson("ana", ["mask_copies", "run_outputs", "credentials"]).catch((e) => e);
    expect(err).toBeInstanceOf(ErasureIncomplete);
    expect(err).toMatchObject({ status: 500, done: ["mask_copies"], remaining: ["run_outputs", "credentials"] });
  });

  it("any other 500 is a plain HttpError", async () => {
    respond(500, { error: "internal error" });
    const err = await credentials.erasePerson("ana", ["credentials"]).catch((e) => e);
    expect(err).not.toBeInstanceOf(ErasureIncomplete);
    expect(err).toMatchObject({ status: 500, message: "internal error" });
  });

  it("a refusal carries the server's sentence", async () => {
    respond(403, { error: "you cannot erase your own records", reason: "erasure_self_refused" });
    await expect(credentials.erasePerson("me", ["run_tasks"])).rejects.toMatchObject({
      status: 403,
      message: "you cannot erase your own records",
      reason: "erasure_self_refused",
    });
  });
});
