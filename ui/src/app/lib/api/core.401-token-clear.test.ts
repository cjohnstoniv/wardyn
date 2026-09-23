/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { getToken, onUnauthorized, setSignedOutHold, setToken, wfetch } from "./core";

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
    setSignedOutHold(false);
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

  // Same X2-F14 leak as probeAuth's, in the one branch that throws instead of
  // returning: the 401 body is never handed to a caller, so nothing drains it
  // and the rejected request stays open on its connection until its deadline.
  it("drains the 401 body it throws over", async () => {
    setToken("t-abc");
    const res = new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 });
    fetchMock.mockResolvedValue(res);
    await expect(wfetch("/runs")).rejects.toMatchObject({ status: 401 });
    expect(res.bodyUsed).toBe(true);
  });

  it("still invokes the shell's onUnauthorized handler", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    setToken("t-abc");
    fetchMock.mockResolvedValue(new Response("", { status: 401 }));
    await expect(wfetch("/runs")).rejects.toBeTruthy();
    expect(handler).toHaveBeenCalledTimes(1);
  });

  // #483: the handler learns whether the refused request was a WRITE, and —
  // only for a request marked as a screen's Save — whose. A save that hit the
  // expiry is never re-sent, so that screen, and no other, has to say so.
  it("#483: the handler is told whether the refused request was a write, and whose Save", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    fetchMock.mockResolvedValue(new Response("", { status: 401 }));
    await expect(wfetch("/runs")).rejects.toBeTruthy();
    expect(handler).toHaveBeenLastCalledWith({ write: false, save: undefined });
    fetchMock.mockResolvedValue(new Response("", { status: 401 }));
    await expect(wfetch("/policies/grade", { method: "POST", body: "{}" })).rejects.toBeTruthy();
    expect(handler).toHaveBeenLastCalledWith({ write: true, save: undefined });
    fetchMock.mockResolvedValue(new Response("", { status: 401 }));
    await expect(wfetch("/workspace-providers", { method: "PUT", body: "{}", save: "providers" })).rejects.toBeTruthy();
    expect(handler).toHaveBeenLastCalledWith({ write: true, save: "providers" });
  });

  // #483: signed out mid-page, no write leaves the tab — whoever holds a
  // session by then (another tab can have signed in as someone else) must
  // never receive the page's draft. Reads still go out.
  it("#483: the signed-out hold refuses every write unsent, and lets reads through", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    setSignedOutHold(true);
    await expect(wfetch("/workspace-providers", { method: "PUT", body: "{}", save: "providers" })).rejects.toMatchObject({
      status: 401,
    });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(handler).toHaveBeenLastCalledWith({ write: true, save: "providers" });
    fetchMock.mockResolvedValue(new Response("[]", { status: 200 }));
    await expect(wfetch("/runs")).resolves.toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // The logout alone passes, per request — the hold itself stays up.
    fetchMock.mockResolvedValue(new Response("", { status: 200 }));
    await expect(wfetch("/auth/logout", { method: "POST", endsSession: true })).resolves.toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await expect(wfetch("/workspace-providers", { method: "PUT", body: "{}" })).rejects.toMatchObject({ status: 401 });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  // Live repro (found via e2e auth.spec.ts, not this file): App.tsx's mount
  // probe fires wfetch("/runs") with WHATEVER token is in storage at that
  // instant — none, on a fresh unauthenticated load. Sign-in then stores a
  // real token a moment later, before that first (unauthenticated) request's
  // 401 has resolved. A clear keyed on a FRESH getToken() read (rather than
  // the token this specific request actually sent) wiped the brand-new,
  // perfectly valid token — the stored session never survived a reload
  // (auth.spec.ts's "reload keeps the session" and 6 siblings, 100%
  // reproducible). The fix compares against the token captured at request-
  // build time; a stale request must never clear a NEWER one.
  it("a stale request's 401 (sent with no/old token) must not clear a NEWER token stored while it was in flight", async () => {
    setToken(null); // the request below goes out with no bearer
    let resolveFetch!: (r: Response) => void;
    fetchMock.mockReturnValue(new Promise<Response>((resolve) => (resolveFetch = resolve)));

    const pending = wfetch("/runs").catch((e) => e);
    // A newer token arrives — e.g. sign-in — WHILE the stale request is
    // still in flight.
    setToken("fresh-token");
    // ...and only now does the stale (no-token) request's 401 land.
    resolveFetch(new Response("", { status: 401, statusText: "Unauthorized" }));
    await pending;

    expect(getToken()).toBe("fresh-token");
  });
});
