/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

const wfetchMock = vi.fn();
vi.mock("./core", async (orig) => ({ ...(await orig<typeof import("./core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

import { HttpError } from "./core";
import { runnerPools } from "./runner-pools";

const refusal = () =>
  new Response(JSON.stringify({ error: "This server does not manage runner pools yet.", reason: "runner_pools_unavailable" }), {
    status: 501,
    headers: { "content-type": "application/json" },
  });

beforeEach(() => wfetchMock.mockReset());

describe("runner pool calls", () => {
  it("hit the routes the server mounts", async () => {
    wfetchMock.mockImplementation(async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } }));
    await runnerPools.list();
    await runnerPools.listMine();
    await runnerPools.get("p 1");
    await runnerPools.addMyRunner("p1", "r1");
    await runnerPools.orgDefaults();
    await runnerPools.myDefaults();
    await runnerPools.setMyDefaults({ pool_id: "p1" });
    const seen = wfetchMock.mock.calls.map(([path, init]) => `${init.method} ${path}`);
    expect(seen).toEqual([
      "GET /runner-pools",
      "GET /me/runner-pools",
      "GET /runner-pools/p%201",
      "PUT /me/runner-pools/p1/runners/r1",
      "GET /runner-pool-defaults",
      "GET /me/runner-pool-defaults",
      "PUT /me/runner-pool-defaults",
    ]);
    expect(JSON.parse(wfetchMock.mock.calls[6][1].body)).toEqual({ pool_id: "p1" });
  });

  it("asks the pool list for one run type, with the caller's own abort signal", async () => {
    wfetchMock.mockImplementation(async () => new Response('{"pools":[]}', { status: 200, headers: { "content-type": "application/json" } }));
    const signal = new AbortController().signal;
    await runnerPools.list("background", signal);
    expect(wfetchMock.mock.calls[0][0]).toBe("/runner-pools?run_type=background");
    expect(wfetchMock.mock.calls[0][1].signal).toBe(signal);
  });

  it("deletes answer 204 with no body", async () => {
    wfetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await runnerPools.removeMyRunner("p1", "r1");
    await runnerPools.clearMyDefaults();
    expect(wfetchMock.mock.calls.map(([path, init]) => `${init.method} ${path}`)).toEqual([
      "DELETE /me/runner-pools/p1/runners/r1",
      "DELETE /me/runner-pool-defaults",
    ]);
  });

  it("surface the server's refusal with its reason, never an empty catalogue", async () => {
    wfetchMock.mockImplementation(async () => refusal());
    await expect(runnerPools.list()).rejects.toMatchObject({ status: 501, reason: "runner_pools_unavailable" });
    await expect(runnerPools.clearMyDefaults()).rejects.toMatchObject({ status: 501, reason: "runner_pools_unavailable" });
    await expect(runnerPools.removeMyRunner("p1", "r1")).rejects.toBeInstanceOf(HttpError);
  });
});
