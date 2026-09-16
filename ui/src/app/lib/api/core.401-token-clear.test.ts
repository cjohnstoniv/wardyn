/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { getToken, onUnauthorized, setToken, wfetch } from "./core";

// F6-F8 — a REAL 401 means the bearer token wfetch just sent was rejected (an
// expired/revoked admin token, or a stale one from another session). Today's
// 401 branch fires the shell's onUnauthorized callback but never clears the
// stored token, so sign-in re-mounts while getToken() still returns the dead
// value — the very next request replays the same rejected bearer. Clearing it
// here, in the one 401 branch every API call routes through, means every
// caller gets this for free.
//
// B6-F2/F6-F8: this must NOT fire on a store-independent failure (a 503
// during a Postgres blip) — adminAuth checks the bearer before the store is
// ever touched, so a bearer-carrying request is never 401'd by a backend
// hiccup. Scoped to the literal 401 branch only.
describe("wfetch — a real 401 clears the stored admin token", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    sessionStorage.clear();
    localStorage.clear();
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    onUnauthorized(() => {});
  });

  it("clears a stored token after a stubbed 401", async () => {
    setToken("t-abc");
    fetchMock.mockResolvedValue(new Response("", { status: 401, statusText: "Unauthorized" }));
    await expect(wfetch("/runs")).rejects.toMatchObject({ status: 401 });
    expect(getToken()).toBeNull();
  });

  it("neg: a cookie-only 401 (no stored token) leaves storage untouched", async () => {
    setToken(null);
    fetchMock.mockResolvedValue(new Response("", { status: 401, statusText: "Unauthorized" }));
    await expect(wfetch("/runs")).rejects.toMatchObject({ status: 401 });
    expect(getToken()).toBeNull();
    expect(sessionStorage.getItem("wardyn_admin_token")).toBeNull();
    expect(localStorage.getItem("wardyn_admin_token")).toBeNull();
  });

  it("still invokes the shell's onUnauthorized handler", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    setToken("t-abc");
    fetchMock.mockResolvedValue(new Response("", { status: 401 }));
    await expect(wfetch("/runs")).rejects.toBeTruthy();
    expect(handler).toHaveBeenCalledTimes(1);
  });
});
