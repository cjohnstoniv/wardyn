/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { access } from "./access";

// R1-F112 — handleUpsertRoleMapping embeds TWO advisory counts beside the
// saved RoleMapping row (internal/api/access.go): tokens_revoked (already
// mirrored) and stale_token_snapshots (F6 type-mirror gap — silently dropped
// by the old client, so the People step could report tokens revoked without
// ever being able to say how many outstanding tokens were affected in the
// first place).
describe("access.upsertMapping — stale_token_snapshots wire mirror", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("surfaces stale_token_snapshots alongside tokensRevoked", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            id: "m-1",
            value: "engineering",
            role: "member",
            stale_token_snapshots: 3,
            tokens_revoked: 2,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    const res = await access.upsertMapping({ value: "engineering", role: "member" });
    expect(res.staleTokenSnapshots).toBe(3);
    expect(res.tokensRevoked).toBe(2);
    // The two counts, not the raw wire keys, ride on `mapping`.
    expect(res.mapping).toEqual({ id: "m-1", value: "engineering", role: "member" });
  });

  it("omits stale_token_snapshots when the write demoted no outstanding token", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ id: "m-1", value: "engineering", role: "member" }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    const res = await access.upsertMapping({ value: "engineering", role: "member" });
    expect(res.staleTokenSnapshots).toBeUndefined();
    expect(res.created).toBe(true);
  });
});
