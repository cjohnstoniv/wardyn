/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { components } from "./components";
import type { ComponentRequest } from "../types";

const ID = "11111111-1111-1111-1111-111111111111";
const req: ComponentRequest = {
  name: "Acme API",
  definition: { hosts: ["api.acme.test"], secrets: [{ secret_name: "acme-key", delivery: { mode: "header", host: "api.acme.test" } }] },
};

describe("components client — routes and verbs", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn().mockImplementation(async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const call = () => ({
    path: String(fetchMock.mock.calls[0][0]),
    method: String(fetchMock.mock.calls[0][1]?.method),
    body: fetchMock.mock.calls[0][1]?.body,
  });

  it.each([
    ["mine", () => components.mine(), "/me/components", "GET", undefined],
    ["saveMine", () => components.saveMine(req), "/me/components", "POST", JSON.stringify(req)],
    ["updateMine", () => components.updateMine(ID, req), `/me/components/${ID}`, "PUT", JSON.stringify(req)],
    ["list", () => components.list(), "/components", "GET", undefined],
    ["put", () => components.put(ID, req), `/components/${ID}`, "PUT", JSON.stringify(req)],
  ])("%s", async (_n, run, path, method, body) => {
    await run();
    const c = call();
    expect(c.path).toBe(`/api/v1${path}`);
    expect(c.method).toBe(method);
    expect(c.body).toBe(body);
  });

  it.each([
    ["deleteMine", () => components.deleteMine(ID), `/me/components/${ID}`],
    ["remove", () => components.remove(ID), `/components/${ID}`],
  ])("%s sends DELETE and accepts 204", async (_n, run, path) => {
    fetchMock.mockImplementation(async () => new Response(null, { status: 204 }));
    await expect(run()).resolves.toBeUndefined();
    const c = call();
    expect(c.path).toBe(`/api/v1${path}`);
    expect(c.method).toBe("DELETE");
  });

  it("a refused delete throws with the status", async () => {
    fetchMock.mockImplementation(async () => new Response(JSON.stringify({ error: "no" }), { status: 404, headers: { "content-type": "application/json" } }));
    await expect(components.deleteMine(ID)).rejects.toMatchObject({ status: 404 });
  });
});
