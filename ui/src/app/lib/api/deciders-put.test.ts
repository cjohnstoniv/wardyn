/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpError, PendingChangeError, setExpectedPrincipal } from "./core";
import { putUnitDeciders, type DeciderRule } from "./deciders";
import { aheadByHours } from "../test-clock";

afterEach(() => {
  vi.unstubAllGlobals();
  setExpectedPrincipal("");
});

const RULES: DeciderRule[] = [{ kinds: ["egress_domain"], who: ["owner", "group:Platform"], max_scope: "once" }];

function stub(status: number, body: unknown) {
  const fn = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify(body), { status }));
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("putUnitDeciders", () => {
  it("PUTs {rules} to the unit's deciders route as the confirmed person", async () => {
    setExpectedPrincipal("ana@example.com");
    const stored = { unit_id: "team a", rules: RULES, updated_by: "ana" };
    const fn = stub(200, stored);
    await expect(putUnitDeciders("team a", RULES)).resolves.toEqual(stored);
    const [url, init] = fn.mock.calls[0];
    expect(String(url)).toContain("/governance/units/team%20a/deciders");
    expect(init?.method).toBe("PUT");
    expect(JSON.parse(String(init?.body))).toEqual({ rules: RULES });
    expect(new Headers(init?.headers).get("X-Wardyn-Expected-Principal")).toBe(encodeURIComponent("ana@example.com"));
  });

  it("throws PendingChangeError on a held write", async () => {
    const change = {
      id: "c1",
      target_kind: "governance_unit_deciders",
      op: "update",
      target_key: "team-a",
      state: "pending",
      proposed_by: "ana",
      proposed_at: aheadByHours(-1),
      expires_at: aheadByHours(71),
      diff: { changed: ["deciders"] },
    };
    stub(202, { pending_change: change });
    const err = await putUnitDeciders("team-a", RULES).catch((e) => e);
    expect(err).toBeInstanceOf(PendingChangeError);
    expect((err as PendingChangeError).change).toEqual(change);
  });

  it("throws HttpError carrying the refusal's reason on a 422", async () => {
    stub(422, { error: "rules[0] is wider than the unit above", reason: "decider_rule_wider_than_parent" });
    const err = await putUnitDeciders("team-a", RULES).catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(422);
    expect((err as HttpError).reason).toBe("decider_rule_wider_than_parent");
  });
});
