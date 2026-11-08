/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two envelope routes: PUT sets a platform's approved groups, DELETE removes the envelope. A held write
// answers 202 with a pending_change body, and neither may read that as a save.
import { describe, it, expect, vi, afterEach } from "vitest";
import { PendingChangeError } from "./core";
import { governance } from "./governance";
import { aheadByHours } from "../test-clock";

afterEach(() => vi.unstubAllGlobals());

const CHANGE = {
  id: "c1",
  target_kind: "governance_envelope",
  op: "set",
  target_key: "root-1",
  state: "pending",
  proposed_by: "ana",
  proposed_at: aheadByHours(-1),
  expires_at: aheadByHours(71),
  diff: { changed: ["approved_groups"] },
};

function stub(res: () => Response) {
  const fn = vi.fn(async (..._a: unknown[]) => res());
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("governance.setEnvelope", () => {
  it("PUTs {approved_groups} to the unit's envelope route and returns the envelope", async () => {
    const env = { unit_id: "root-1", approved_groups: ["g1", "g2"], set_by: "ana", set_at: aheadByHours(0) };
    const fn = stub(() => new Response(JSON.stringify(env), { status: 200 }));
    await expect(governance.setEnvelope("root-1", ["g1", "g2"])).resolves.toEqual(env);
    const [url, init] = fn.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/units/root-1/envelope");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual({ approved_groups: ["g1", "g2"] });
  });

  it("throws PendingChangeError on a 202", async () => {
    stub(() => new Response(JSON.stringify({ pending_change: CHANGE }), { status: 202 }));
    const err = await governance.setEnvelope("root-1", ["g1"]).catch((e) => e);
    expect(err).toBeInstanceOf(PendingChangeError);
    expect((err as PendingChangeError).change).toEqual(CHANGE);
  });
});

describe("governance.clearEnvelope", () => {
  it("DELETEs the route and resolves on a 204", async () => {
    const fn = stub(() => new Response(null, { status: 204 }));
    await expect(governance.clearEnvelope("root-1")).resolves.toBeUndefined();
    const [url, init] = fn.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/units/root-1/envelope");
    expect(init.method).toBe("DELETE");
  });

  it("throws PendingChangeError on a 202 with a pending_change", async () => {
    stub(() => new Response(JSON.stringify({ pending_change: CHANGE }), { status: 202 }));
    await expect(governance.clearEnvelope("root-1")).rejects.toBeInstanceOf(PendingChangeError);
  });
});
