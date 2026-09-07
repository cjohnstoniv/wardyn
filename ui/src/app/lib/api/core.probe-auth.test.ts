/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { onUnauthorized, probeAuth, setToken } from "./core";

// R4/F027 — probeAuth must tell THREE things apart, because the console spends
// each of them differently:
//
//   a real 401       → the human is signed out; the gate is the right screen
//                      and "sign in" is the right advice.
//   a daemon 5xx     → the human's credentials were never judged. The daemon
//   a dead network     answered nothing (or nothing useful), so telling them
//                      they are signed out is a statement the console cannot
//                      support — and it is the ONE state where the console's
//                      reachability banner should be up.
//
// It used to return a plain boolean, so all three were `false`, App.tsx turned
// that into setAuth("unauthed"), and the sign-in gate rendered with no outage
// explanation anywhere on it. sign-in.tsx's submitToken had already had to make
// this split by hand for exactly the same reason (sign-in.tsx:116-123).
describe("probeAuth — a rejected caller, a broken daemon and a dead network are not the same answer", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    sessionStorage.clear();
    localStorage.clear();
    setToken(null);
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    onUnauthorized(() => {});
  });

  it("a protected row that comes back is 'authed'", async () => {
    fetchMock.mockResolvedValue(new Response("[]", { status: 200 }));
    expect(await probeAuth()).toBe("authed");
  });

  it("a REAL 401 is 'unauthed' — the only answer that means the caller is signed out", async () => {
    fetchMock.mockResolvedValue(new Response("", { status: 401, statusText: "Unauthorized" }));
    expect(await probeAuth()).toBe("unauthed");
  });

  it("a daemon 5xx is 'unreachable', NOT 'unauthed' — nothing judged the credentials", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "database unavailable" }), { status: 500 }),
    );
    expect(await probeAuth()).toBe("unreachable");
  });

  it("a transport failure is 'unreachable' — the request never reached a judge at all", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    expect(await probeAuth()).toBe("unreachable");
  });

  it("a 503 during a rolling restart does not report a valid session as signed out", async () => {
    setToken("a-perfectly-good-token");
    fetchMock.mockResolvedValue(new Response("", { status: 503, statusText: "Service Unavailable" }));
    expect(await probeAuth()).toBe("unreachable");
    // …and the token it was carrying is untouched: a blip must not discard a
    // credential that was never rejected.
    expect(sessionStorage.getItem("wardyn_admin_token")).toBe("a-perfectly-good-token");
  });
});
