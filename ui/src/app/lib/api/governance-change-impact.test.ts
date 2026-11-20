/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The change-impact client: the GET beside the change's own.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { governance } from "./governance";

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("change impact client", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("getChangeImpact GETs the impact route of the change and returns the body", async () => {
    const body = { computed: "tree", as_of: "2026-01-01T00:00:00Z", people: [], live_runs_total: 0 };
    fetchMock.mockResolvedValueOnce(json(body));
    const got = await governance.getChangeImpact("c/1");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/changes/c%2F1/impact");
    expect(init.method).toBe("GET");
    expect(got).toEqual(body);
  });

  it("getChangeImpact reaches the caller as an HttpError on a 409", async () => {
    fetchMock.mockResolvedValueOnce(json({ error: "not pending", reason: "governance_change_not_pending" }, 409));
    await expect(governance.getChangeImpact("c1")).rejects.toMatchObject({ status: 409 });
  });
});
