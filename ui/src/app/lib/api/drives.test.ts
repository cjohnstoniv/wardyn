/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { drives } from "./drives";

// R4/F092 — getDrives() is a PROJECTION, not a pass-through: it rebuilds the
// body as a fresh object literal so nil Go slices become arrays every caller can
// map over. That shape is also how a declared key disappears — anything the
// literal does not name is dropped before a consumer can see it, silently and
// with the type still claiming otherwise.
//
// `grant_total` is the one that matters. handleGetUserDrives bounds the grants
// read at maxListLimit because user_drive_grants holds one row per SUBJECT — its
// size is the deployment's headcount — and ships the total beside the page
// exactly so "this is all of them" can be told from "this is the first page"
// (internal/api/user_drives.go:99-104). Dropped here, the Allocations table
// rendered one bounded window as if it were every allocation.
describe("drives.getDrives() — the disclosure keys survive the projection", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  function body(over: Record<string, unknown> = {}) {
    return new Response(
      JSON.stringify({
        drives: null,
        grants: null,
        host_roots_configured: true,
        runner_target: "k8s",
        ...over,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }

  it("carries grant_total through, so a bounded page is knowable as one", async () => {
    fetchMock.mockResolvedValueOnce(
      body({ grants: [{ id: "g1", subject_type: "user", subject: "a@b.c", drive_id: "d1", priority: 0 }], grant_total: 4000 }),
    );
    const snap = await drives.getDrives();
    expect(snap.grant_total).toBe(4000);
    // The page itself is untouched — the total is ABOUT the page, not instead of it.
    expect(snap.grants).toHaveLength(1);
  });

  it("leaves grant_total undefined for a pre-0.7 daemon that never sends it", async () => {
    fetchMock.mockResolvedValueOnce(body());
    const snap = await drives.getDrives();
    // undefined is "unknown", and unknown must not read as 0 — a 0 total under a
    // non-empty page would be a truncation claim nobody made.
    expect(snap.grant_total).toBeUndefined();
    expect(snap.grants).toEqual([]);
    expect(snap.drives).toEqual([]);
  });
});
