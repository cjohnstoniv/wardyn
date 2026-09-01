/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one branch lib/api/directory.ts owns: ABSENT vs BROKEN. The console
// renders those two completely differently — a plain text input with no error
// surface, versus LOOKUP_FAILED — so the discrimination has to be on the
// endpoint's distinct 503 CODE and nothing else (a bare 503 from a proxy is a
// failure, not a feature that was never enabled). Component tests mock this
// module away, so this is where the wire contract is pinned.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// Fresh module per test: the unconfigured answer LATCHES for the session, and
// a latch is only honest if it starts unset.
const fresh = async () => {
  vi.resetModules();
  return import("./directory");
};

const json = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("directory.search", () => {
  it("asks the endpoint for the query and the kind, and returns the entries", async () => {
    const entry = { display_name: "Alice Ng", claim_value: "alice@corp.example", kind: "user" as const };
    fetchMock.mockResolvedValue(json({ results: [entry] }, 200));
    const { directory } = await fresh();

    expect(await directory.search("ali ng", "user")).toEqual([entry]);
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain("/api/v1/access/directory/search");
    expect(url).toContain("q=ali%20ng");
    expect(url).toContain("type=user");
  });

  it("defaults to the kind-LESS search — the People step's Value field is one input for three sources", async () => {
    fetchMock.mockResolvedValue(json({ results: [] }, 200));
    const { directory } = await fresh();

    expect(await directory.search("eng")).toEqual([]);
    expect(String(fetchMock.mock.calls[0][0])).toContain("type=any");
  });

  it("the unconfigured 503 is null — never a throw — and it stops asking", async () => {
    fetchMock.mockResolvedValue(json({ code: "directory_unconfigured", error: "no directory provider is configured" }, 503));
    const { directory } = await fresh();

    expect(await directory.search("eng")).toBeNull();
    expect(await directory.search("eng-team")).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(1); // latched, so a keystroke costs nothing
  });

  it("a 503 WITHOUT that code is broken, not absent", async () => {
    fetchMock.mockResolvedValue(json({ error: "upstream unavailable" }, 503));
    const { directory } = await fresh();

    await expect(directory.search("eng")).rejects.toThrow("upstream unavailable");
    // …and it must not have latched: the next keystroke asks again.
    fetchMock.mockResolvedValue(json({ results: [] }, 200));
    expect(await directory.search("eng")).toEqual([]);
  });

  it("a connector failure throws, so the field can say so", async () => {
    fetchMock.mockResolvedValue(json({ error: "directory search failed" }, 500));
    const { directory } = await fresh();

    await expect(directory.search("eng")).rejects.toThrow("directory search failed");
  });
});
