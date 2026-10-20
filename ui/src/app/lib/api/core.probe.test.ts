/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which probe answers are worth asking again: a transient failure (a network error, a timeout, a 5xx, a 429)
// is; a 401, and a 403 or another 4xx, are final. `probe` and `reason` read as they always have.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { probeAuthWithReason } from "./core";

const fetchMock = vi.fn<typeof fetch>();
beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const answer = (status: number) => fetchMock.mockResolvedValue(new Response("{}", { status }));

it("a 200 is authed", async () => {
  answer(200);
  expect(await probeAuthWithReason()).toEqual({ probe: "authed", reason: "", transient: false });
});

it.each([500, 502, 503, 429])("a %i is unreachable and transient", async (status) => {
  answer(status);
  expect(await probeAuthWithReason()).toEqual({ probe: "unreachable", reason: "", transient: true });
});

it("a dropped connection is unreachable and transient", async () => {
  fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
  expect(await probeAuthWithReason()).toEqual({ probe: "unreachable", reason: "", transient: true });
});

it.each([403, 404])("a %i is unreachable and final", async (status) => {
  answer(status);
  expect(await probeAuthWithReason()).toEqual({ probe: "unreachable", reason: "", transient: false });
});

it("a 401 is unauthed and final", async () => {
  answer(401);
  const r = await probeAuthWithReason();
  expect(r.probe).toBe("unauthed");
  expect(r.transient).toBe(false);
});
