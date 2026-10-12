/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpError } from "./core";
import { runners } from "./runners";

afterEach(() => vi.restoreAllMocks());

const json = (body: unknown, init: ResponseInit & { truncated?: boolean } = {}) =>
  new Response(JSON.stringify(body), { status: init.status ?? 200, headers: { "Content-Type": "application/json", ...(init.truncated ? { "X-Wardyn-Truncated": "true" } : {}) } });
const call = (spy: ReturnType<typeof vi.spyOn>, n = 0) => {
  const [url, init] = spy.mock.calls[n] as [string, RequestInit | undefined];
  return { path: String(url).replace(/^.*\/api\/v1/, ""), method: init?.method, body: init?.body ? JSON.parse(String(init.body)) : undefined };
};

describe("runners api", () => {
  it("reads every page of a list while the server says more remain", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json([{ id: "a" }, { id: "b" }], { truncated: true })).mockResolvedValueOnce(json([{ id: "c" }]));
    expect((await runners.list("revoked")).map((r) => r.id)).toEqual(["a", "b", "c"]);
    expect(call(spy, 0).path).toMatch(/^\/runners\?state=revoked&limit=\d+&offset=0$/);
    expect(call(spy, 1).path).toMatch(/&offset=2$/);
  });

  it("a null list is an empty one", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(json(null));
    expect(await runners.listMine()).toEqual([]);
  });

  // A7: the not-mine door is the one query the console adds to a revoke.
  it("a person's revoke sends ?reason=not_mine only when it is marked, and the admin revoke is DELETE /runners/{id}", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(json({ id: "x", state: "revoked", runs_killed: 0 })));
    await runners.revokeMine("r 1", true);
    await runners.revokeMine("r 1");
    await runners.revoke("r 1");
    expect(call(spy, 0)).toMatchObject({ path: "/me/runners/r%201?reason=not_mine", method: "DELETE" });
    expect(call(spy, 1)).toMatchObject({ path: "/me/runners/r%201", method: "DELETE" });
    expect(call(spy, 2)).toMatchObject({ path: "/runners/r%201", method: "DELETE" });
  });

  it("mints for a named person, mints for oneself, claims with the fingerprint, and switches", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(json({}, { status: 201 })));
    await runners.mint("alice@example.com");
    await runners.mintMine();
    await runners.claim("r1", "SHA256:abc");
    await runners.setEnabled(true);
    await runners.revokeToken("t1").catch(() => {});
    expect(call(spy, 0)).toMatchObject({ path: "/runners/tokens", method: "POST", body: { owner: "alice@example.com" } });
    expect(call(spy, 1)).toMatchObject({ path: "/me/runners/tokens", method: "POST", body: {} });
    expect(call(spy, 2)).toMatchObject({ path: "/me/runners/r1/claim", method: "POST", body: { fingerprint: "SHA256:abc" } });
    expect(call(spy, 3)).toMatchObject({ path: "/runners/settings", method: "PUT", body: { enabled: true } });
    expect(call(spy, 4)).toMatchObject({ path: "/runners/tokens/t1", method: "DELETE" });
  });

  it("a refusal keeps the server's sentence and reason", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ error: "runners are off", reason: "runners_disabled" }, { status: 403 }));
    const err = await runners.listMine().catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect(err).toMatchObject({ status: 403, message: "runners are off", reason: "runners_disabled" });
  });
});
