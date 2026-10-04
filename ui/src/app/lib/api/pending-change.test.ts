/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A covered governance write the server holds for a second person answers 202 with a `pending_change` body.
// No write site may read that as a save: asJson throws PendingChangeError for it, and the delete routes (which
// read no body and so never reach asJson) throw it through throwIfPending. Every covered site in the four
// api modules is pinned here; a site added later through asJson is covered by the first two tests.
import { describe, it, expect, vi, afterEach } from "vitest";
import { access } from "./access";
import { asJson, PendingChangeError } from "./core";
import { governance, type GovernanceProfileInput } from "./governance";
import { permissions } from "./permissions";
import { userTypes } from "./user-types";
import { aheadByHours } from "../test-clock";

afterEach(() => vi.unstubAllGlobals());

const CHANGE = {
  id: "c1",
  target_kind: "governance_profile",
  op: "update",
  target_key: "team-a",
  state: "pending",
  proposed_by: "ana",
  proposed_at: aheadByHours(-1),
  expires_at: aheadByHours(71),
  diff: { changed: ["ceiling.allowed_domains"] },
};

function stubFetch(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

describe("asJson on a 202", () => {
  it("throws PendingChangeError carrying the pending_change body", async () => {
    const res = new Response(JSON.stringify({ pending_change: CHANGE }), { status: 202 });
    const err = await asJson(res).catch((e) => e);
    expect(err).toBeInstanceOf(PendingChangeError);
    expect((err as PendingChangeError).change).toEqual(CHANGE);
  });

  it("returns a 202 body without pending_change unchanged", async () => {
    const res = new Response(JSON.stringify({ accepted: true }), { status: 202 });
    await expect(asJson(res)).resolves.toEqual({ accepted: true });
  });

  it("does not take a 200 for a held change, whatever its body names", async () => {
    const res = new Response(JSON.stringify({ pending_change: CHANGE }), { status: 200 });
    await expect(asJson(res)).resolves.toEqual({ pending_change: CHANGE });
  });
});

const PROFILE_INPUT: GovernanceProfileInput = {
  name: "team-a",
  ceiling: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
};

const SITES: [string, () => Promise<unknown>][] = [
  ["governance.createProfile", () => governance.createProfile(PROFILE_INPUT)],
  ["governance.updateProfile", () => governance.updateProfile("p1", PROFILE_INPUT)],
  ["governance.deleteProfile", () => governance.deleteProfile("p1")],
  [
    "governance.upsertAssignment",
    () => governance.upsertAssignment({ subject_type: "group", subject: "g", profile_id: "p1", priority: 0 }),
  ],
  ["governance.deleteAssignment", () => governance.deleteAssignment("a1")],
  [
    "permissions.upsertGrant",
    () =>
      permissions.upsertGrant({ subject_type: "all", subject: "", capability: "egress_host", value: "x", effect: "allow" }),
  ],
  ["permissions.deleteGrant", () => permissions.deleteGrant("g1")],
  ["permissions.putEnforcement", () => permissions.putEnforcement({ egress_host: true })],
  ["permissions.putAvailability", () => permissions.putAvailability("image", "ghcr.io/x/y:1", true)],
  ["access.upsertMapping", () => access.upsertMapping({ value: "v", role: "admin" })],
  ["access.deleteMapping", () => access.deleteMapping("m1")],
  ["userTypes.createUserType", () => userTypes.createUserType({ name: "t", description: "", priority: 1 })],
  ["userTypes.updateUserType", () => userTypes.updateUserType("t1", { name: "t", description: "", priority: 1 })],
  ["userTypes.deleteUserType", () => userTypes.deleteUserType("t1")],
];

describe("every covered write site", () => {
  it.each(SITES)("%s throws PendingChangeError on a 202, and never resolves as a save", async (_name, call) => {
    stubFetch(202, { pending_change: CHANGE });
    const err = await call().then(
      () => null,
      (e) => e,
    );
    expect(err).toBeInstanceOf(PendingChangeError);
    expect((err as PendingChangeError).change.id).toBe("c1");
  });
});

describe("governance changes", () => {
  it("lists the pending changes, coercing a null body to an empty list", async () => {
    stubFetch(200, null);
    await expect(governance.listChanges()).resolves.toEqual([]);
    stubFetch(200, [CHANGE]);
    await expect(governance.listChanges()).resolves.toEqual([CHANGE]);
  });

  it("sends the optional reason on a reject only when there is one", async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify(CHANGE), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await governance.rejectChange("c1", "");
    await governance.rejectChange("c1", "not now");
    const bodies = fetchMock.mock.calls.map((c) => (c as unknown as [string, RequestInit])[1].body);
    expect(bodies).toEqual(["{}", JSON.stringify({ reason: "not now" })]);
  });
});
