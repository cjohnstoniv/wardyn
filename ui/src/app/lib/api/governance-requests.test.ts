/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { HttpError, PendingChangeError } from "./core";
import { requests } from "./governance-requests";

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const change = { id: "c1", state: "pending" };
const view = { ...change, kind: "elevation", unit_id: "u1", unit_name: "Payments", duration: "4h", mine: true, decidable: false, deciders: [] };

describe("requests client", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const lastCall = () => {
    const [url, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as [string, RequestInit];
    return { url: String(url), init };
  };

  it("list reads the list, with nil slices as arrays", async () => {
    fetchMock.mockResolvedValueOnce(json(200, { requests: null, offered: null, image_offered: false }));
    const out = await requests.list();
    expect(out).toEqual({ requests: [], offered: [], image_offered: false });
    expect(lastCall().url).toContain("/governance/requests");
  });

  it("list is null on a 404, the older-server signal", async () => {
    fetchMock.mockResolvedValueOnce(json(404, { error: "not found" }));
    expect(await requests.list()).toBeNull();
  });

  it("list throws on a 500", async () => {
    fetchMock.mockResolvedValueOnce(json(500, { error: "boom" }));
    await expect(requests.list()).rejects.toBeInstanceOf(HttpError);
  });

  it("create returns the request from a 202 and does not throw PendingChangeError", async () => {
    fetchMock.mockResolvedValueOnce(json(202, { pending_change: change, request: view }));
    const body = { kind: "elevation" as const, unit_id: "u1", duration: "4h", reason: "deploy" };
    const out = await requests.create(body);
    expect(out).toEqual(view);
    const { url, init } = lastCall();
    expect(url).toContain("/governance/requests");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual(body);
  });

  it("create never surfaces the held change as an error", async () => {
    fetchMock.mockResolvedValueOnce(json(202, { pending_change: change, request: view }));
    await expect(requests.create({ kind: "elevation", reason: "r" })).resolves.not.toBeInstanceOf(PendingChangeError);
  });

  it("create throws HttpError with the reason on a 400", async () => {
    fetchMock.mockResolvedValueOnce(json(400, { error: "Payments allows at most 4h.", reason: "elevation_too_long" }));
    const err = await requests.create({ kind: "elevation", unit_id: "u1", duration: "8h", reason: "r" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).reason).toBe("elevation_too_long");
    expect((err as HttpError).status).toBe(400);
    expect((err as HttpError).message).toBe("Payments allows at most 4h.");
  });

  it("approve posts to the approve route", async () => {
    fetchMock.mockResolvedValueOnce(json(200, change));
    expect(await requests.approve("c 1")).toEqual(change);
    const { url, init } = lastCall();
    expect(url).toContain("/governance/requests/c%201/approve");
    expect(init.method).toBe("POST");
  });

  it("reject sends {} for an empty reason and the reason otherwise", async () => {
    fetchMock.mockResolvedValueOnce(json(200, change)).mockResolvedValueOnce(json(200, change));
    await requests.reject("c1", "");
    let call = lastCall();
    expect(call.url).toContain("/governance/requests/c1/reject");
    expect(JSON.parse(String(call.init.body))).toEqual({});
    await requests.reject("c1", "no");
    call = lastCall();
    expect(JSON.parse(String(call.init.body))).toEqual({ reason: "no" });
  });
});
