/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { workloadBindings, workloads } from "./workloads";

// Each call is reached through fetch stubbed at the global, so the request the console really sends is pinned.
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stub(response: Response) {
  const fetchMock = vi.fn().mockResolvedValue(response);
  vi.stubGlobal("fetch", fetchMock);
  return () => {
    const [url, init] = fetchMock.mock.calls[0];
    return { url: String(url), method: init.method as string, body: init.body as string | undefined };
  };
}

describe("workloads - request shape", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("list GETs /workloads and turns null into []", async () => {
    const sent = stub(json(null));
    expect(await workloads.list()).toEqual([]);
    expect(sent()).toMatchObject({ url: "/api/v1/workloads?limit=1000", method: "GET" });
  });

  it("history GETs /workloads/{name}/history, with the owner when given", async () => {
    let sent = stub(json(null));
    expect(await workloads.history("billing-api")).toEqual([]);
    expect(sent()).toMatchObject({ url: "/api/v1/workloads/billing-api/history?limit=1000", method: "GET" });
    sent = stub(json([{ time: "t", action: "launch" }]));
    expect(await workloads.history("billing-api", "service:billing")).toHaveLength(1);
    expect(sent()).toMatchObject({ url: "/api/v1/workloads/billing-api/history?owner=service%3Abilling&limit=1000", method: "GET" });
  });

  it("get, restart and remove name the owner only when given", async () => {
    let sent = stub(json({ name: "a/b" }));
    await workloads.get("a/b");
    expect(sent()).toMatchObject({ url: "/api/v1/workloads/a%2Fb", method: "GET" });
    sent = stub(json({ workload: {}, outcome: "unchanged", launch: { state: "launched" } }));
    await workloads.restart("billing-api", "service:billing");
    expect(sent()).toMatchObject({ url: "/api/v1/workloads/billing-api/restart?owner=service%3Abilling", method: "POST" });
    sent = stub(new Response(null, { status: 204 }));
    await workloads.remove("triage-bot");
    expect(sent()).toMatchObject({ url: "/api/v1/workloads/triage-bot", method: "DELETE" });
  });
});

describe("workloadBindings - request shape", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("list GETs /workload-bindings", async () => {
    const sent = stub(json([{ id: "b1" }]));
    expect(await workloadBindings.list()).toHaveLength(1);
    expect(sent()).toMatchObject({ url: "/api/v1/workload-bindings?limit=1000", method: "GET" });
  });

  it("create POSTs pool, namespace and service", async () => {
    const sent = stub(json({ id: "b1" }, 201));
    await workloadBindings.create({ pool: "Platform services", namespace: "team-payments", service: "billing" });
    expect(sent()).toEqual({
      url: "/api/v1/workload-bindings",
      method: "POST",
      body: '{"pool":"Platform services","namespace":"team-payments","service":"billing"}',
    });
  });

  it("remove DELETEs the binding by id", async () => {
    const sent = stub(new Response(null, { status: 204 }));
    await workloadBindings.remove("b/1");
    expect(sent()).toMatchObject({ url: "/api/v1/workload-bindings/b%2F1", method: "DELETE" });
  });
});
