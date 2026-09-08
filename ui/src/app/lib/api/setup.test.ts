/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { setup } from "./setup";
import { HttpError, onUnauthorized, wfetch } from "./core";
import type { SetupStatus } from "../types";

// setup.getSetupStatus() must be endpoint-less-build tolerant: an older control
// plane (or any deployment without the /setup/status route) answers 404, so a
// 404 (or any other failure) must degrade to a permissive
// READY_FALLBACK — ready:true — so the auto-open effect in App.tsx never opens
// the Getting-started wizard against a control plane that can't answer it. A
// 401 is the one exception: wfetch already routes it through onUnauthorized
// and throws, and that must keep propagating unchanged.
describe("setup.getSetupStatus()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const sample: SetupStatus = {
    ready: false,
    checks: [{ id: "gvisor", label: "gVisor runtime", status: "warn" }],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false },
  };

  it("parses and returns the status on a 200", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(sample), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await setup.getSetupStatus();
    expect(res).toEqual(sample);
  });

  it("returns a permissive ready:true fallback on a 404 (endpoint not built yet)", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    const res = await setup.getSetupStatus();
    expect(res.ready).toBe(true);
    expect(res.auth.mode).toBe("local");
    expect(res.checks).toEqual([]);
  });

  it("returns the ready:true fallback on any other non-ok status", async () => {
    fetchMock.mockResolvedValueOnce(new Response("boom", { status: 500 }));
    const res = await setup.getSetupStatus();
    expect(res.ready).toBe(true);
  });

  it("returns the ready:true fallback when the network throws", async () => {
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    const res = await setup.getSetupStatus();
    expect(res.ready).toBe(true);
  });

  it("propagates a 401 through onUnauthorized instead of falling back", async () => {
    // R4/F116: the NAME of this test is the whole behaviour, and it used to
    // assert only the throw — the handler half went unread, so deleting
    // `_unauthorized?.()` from wfetch left this green (and the whole suite with
    // it). Both halves now, because the fallback and the sign-out are different
    // guarantees: the throw is "getSetupStatus did not answer ready:true for a
    // caller the daemon refused", the handler is "the console went back to the
    // gate".
    const seen: string[] = [];
    onUnauthorized(() => seen.push("unauthorized"));
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));
    await expect(setup.getSetupStatus()).rejects.toBeInstanceOf(HttpError);
    expect(seen).toEqual(["unauthorized"]);
  });
});

// R4/F116 — wfetch's 401 arm is the console's ONE mid-session expiry path: every
// call in every module goes through it, and App.tsx wires the handler to
// setAuth("unauthed"), the sign-in gate. Nothing in either tier pinned it —
// deleting `_unauthorized?.()` left 108 files / 1823 tests green, and no
// Playwright spec revokes a session mid-run — so an expired SSO session would
// have left an operator clicking a console that answered 401 to everything and
// never returned them to a door they could open.
//
// Pinned at the seam itself rather than only through one module's caller: it is
// wfetch's contract, not setup's.
describe("wfetch — a 401 is BOTH a throw and a sign-out (R4/F116)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    onUnauthorized(() => {});
  });

  it("fires the registered handler and throws HttpError(401)", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));

    await expect(wfetch("/runs", { method: "GET" })).rejects.toMatchObject({
      name: "HttpError",
      status: 401,
    });
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("does NOT fire it for any other refusal — a 403 is not an expired session", async () => {
    // Negative control, and a real distinction: a 403 means "this caller may not
    // do that", and bouncing them to the sign-in gate would be advice to
    // re-authenticate as the same person who was just refused.
    const handler = vi.fn();
    onUnauthorized(handler);
    fetchMock.mockResolvedValueOnce(new Response("forbidden", { status: 403 }));

    const res = await wfetch("/runs", { method: "GET" });
    expect(res.status).toBe(403);
    expect(handler).not.toHaveBeenCalled();
  });

  it("does not fire it on a 200", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);
    fetchMock.mockResolvedValueOnce(new Response("[]", { status: 200 }));

    await wfetch("/runs", { method: "GET" });
    expect(handler).not.toHaveBeenCalled();
  });
});
