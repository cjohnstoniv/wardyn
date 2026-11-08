/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { PendingChangeError } from "./core";
import { requests } from "./governance-requests";

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const REF = `ghcr.io/acme/build-tools@sha256:${"a".repeat(64)}`;
const change = { id: "c1", state: "pending" };
const view = { ...change, kind: "image", image: REF, unit_name: "Payments", mine: true, decidable: false, deciders: [] };

describe("requests client: an image request", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("posts exactly the image, the reason and the kind, with no unit or duration", async () => {
    fetchMock.mockResolvedValueOnce(json(202, { pending_change: change, request: view }));
    const out = await requests.create({ kind: "image", image: REF, reason: "build step" });
    expect(out).toEqual(view);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(String(url)).toContain("/governance/requests");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ kind: "image", image: REF, reason: "build step" });
  });

  it("carries the ticket when there is one, and does not throw the held change", async () => {
    fetchMock.mockResolvedValueOnce(json(202, { pending_change: change, request: view }));
    const body = { kind: "image" as const, image: REF, reason: "build step", ticket: "INC-2210" };
    await expect(requests.create(body)).resolves.not.toBeInstanceOf(PendingChangeError);
    expect(JSON.parse(String((fetchMock.mock.calls[0] as [string, RequestInit])[1].body))).toEqual(body);
  });
});
