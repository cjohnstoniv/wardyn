/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api, HttpError } from "./api";
import type { SetupStatus } from "./types";

// api.getSetupStatus() must be endpoint-less-build tolerant: the /setup/status
// route is being built concurrently against the same frozen SetupStatus
// contract, so a 404 (or any other failure) must degrade to a permissive
// READY_FALLBACK — ready:true — so the auto-open effect in App.tsx never opens
// the Getting-started wizard against a control plane that can't answer it. A
// 401 is the one exception: wfetch already routes it through onUnauthorized
// and throws, and that must keep propagating unchanged.
describe("api.getSetupStatus()", () => {
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
    composer: { enabled: false, backends: [] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    restart_required: false,
    has_runs: false,
    platform: { os: "linux", wsl: false },
  };

  it("parses and returns the status on a 200", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(sample), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await api.getSetupStatus();
    expect(res).toEqual(sample);
  });

  it("returns a permissive ready:true fallback on a 404 (endpoint not built yet)", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    const res = await api.getSetupStatus();
    expect(res.ready).toBe(true);
    expect(res.auth.mode).toBe("local");
    expect(res.checks).toEqual([]);
  });

  it("returns the ready:true fallback on any other non-ok status", async () => {
    fetchMock.mockResolvedValueOnce(new Response("boom", { status: 500 }));
    const res = await api.getSetupStatus();
    expect(res.ready).toBe(true);
  });

  it("returns the ready:true fallback when the network throws", async () => {
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    const res = await api.getSetupStatus();
    expect(res.ready).toBe(true);
  });

  it("propagates a 401 through onUnauthorized instead of falling back", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));
    await expect(api.getSetupStatus()).rejects.toBeInstanceOf(HttpError);
  });
});
