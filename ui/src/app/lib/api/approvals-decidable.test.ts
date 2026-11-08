/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two reads behind the Approvals screen's "Waiting on you" tab, against the real module with fetch stubbed.
// Run: cd ui && pnpm vitest run src/app/lib/api/approvals-decidable.test.ts

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { approvals } from "./approvals";

let fetchMock: ReturnType<typeof vi.fn>;
const answer = (status: number, body: unknown, headers: Record<string, string> = {}) => {
  fetchMock = vi.fn().mockImplementation(
    async () =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", ...headers } }),
  );
  vi.stubGlobal("fetch", fetchMock);
};
const sentPath = (): string => String(fetchMock.mock.calls[0][0]);

beforeEach(() => answer(200, []));
afterEach(() => vi.unstubAllGlobals());

describe("approvals.listDecidableByMe", () => {
  it("asks for decidable_by=me with no state and no limit, and returns the rows", async () => {
    answer(200, [{ id: "ap_1", requested_by: "dana@acme.example" }]);
    const out = await approvals.listDecidableByMe();
    const url = new URL(sentPath(), "http://x");
    expect(url.pathname.endsWith("/approvals")).toBe(true);
    expect(url.searchParams.get("decidable_by")).toBe("me");
    expect(url.searchParams.has("state")).toBe(false);
    expect(url.searchParams.has("limit")).toBe(false);
    expect(out.rows.map((r) => r.id)).toEqual(["ap_1"]);
    expect(out.truncated).toBe(false);
  });

  it("reports truncated when the server sets X-Wardyn-Truncated: true", async () => {
    answer(200, [], { "X-Wardyn-Truncated": "true" });
    expect((await approvals.listDecidableByMe()).truncated).toBe(true);
  });

  it("reports not truncated when the header is absent", async () => {
    answer(200, [{ id: "ap_1" }]);
    expect((await approvals.listDecidableByMe()).truncated).toBe(false);
  });
});

describe("approvals.knowsDecidableBy", () => {
  it("is true only for a 400 invalid_decidable_by", async () => {
    answer(400, { error: "decidable_by must be me", reason: "invalid_decidable_by" });
    expect(await approvals.knowsDecidableBy()).toBe(true);
    expect(new URL(sentPath(), "http://x").searchParams.get("decidable_by")).toBe("x");
  });

  it("is false for a 200 (an older server ignores the parameter)", async () => {
    answer(200, []);
    expect(await approvals.knowsDecidableBy()).toBe(false);
  });

  it("is false for a 400 with another reason", async () => {
    answer(400, { error: "bad", reason: "invalid_limit" });
    expect(await approvals.knowsDecidableBy()).toBe(false);
  });

  it("is false for any other failure", async () => {
    answer(500, { error: "boom" });
    expect(await approvals.knowsDecidableBy()).toBe(false);
  });
});
