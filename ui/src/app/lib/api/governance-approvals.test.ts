/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The approvals client: approve with a reason, take an approval back, and read and replace the approval counts.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { governance } from "./governance";
import { PendingChangeError } from "./core";
import type { ApprovalCounts, ApprovalCountsWrite } from "../types/approval-counts";
import { aheadByHours } from "../test-clock";

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const change = {
  id: "c1",
  target_kind: "governance_unit",
  op: "update",
  target_key: "k",
  state: "pending",
  proposed_by: "alice",
  proposed_at: aheadByHours(0),
  expires_at: aheadByHours(72),
  diff: { changed: ["name"] },
};

const counts: ApprovalCounts = {
  minimum: 2,
  highest: 3,
  default: { required: 2, require_security: false },
  kinds: [
    {
      kind: "governance_unit",
      own: true,
      required: 3,
      require_security: true,
      approvers: "security_or_unit_admin",
      pool: { status: "short", count: 2, unit_name: "Data Science" },
    },
  ],
};

describe("approvals client", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("approveChange sends no body when there is no reason", async () => {
    fetchMock.mockResolvedValueOnce(json({ ...change, state: "applied" }));
    await governance.approveChange("c/1");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/changes/c%2F1/approve");
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
  });

  it("approveChange sends the reason only when it is given, and reads a change that is still pending", async () => {
    const pending = { ...change, approvals: { required: 2, require_security: false, have: 1 } };
    fetchMock.mockResolvedValueOnce(json(pending));
    const got = await governance.approveChange("c1", "checked the diff");
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ reason: "checked the diff" });
    expect(got.state).toBe("pending");
    expect(got.approvals?.have).toBe(1);
    fetchMock.mockResolvedValueOnce(json(change));
    await governance.approveChange("c1", "");
    expect((fetchMock.mock.calls[1] as [string, RequestInit])[1].body).toBeUndefined();
  });

  it("takeBackApproval DELETEs the approval of the change and returns the change", async () => {
    fetchMock.mockResolvedValueOnce(json(change));
    const got = await governance.takeBackApproval("c/1");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/changes/c%2F1/approval");
    expect(init.method).toBe("DELETE");
    expect(got.id).toBe("c1");
  });

  it("takeBackApproval reaches the caller as an HttpError on a 404", async () => {
    fetchMock.mockResolvedValueOnce(json({ error: "none", reason: "governance_change_approval_not_found" }, 404));
    await expect(governance.takeBackApproval("c1")).rejects.toMatchObject({ status: 404 });
  });

  it("getApprovalCounts GETs the counts", async () => {
    fetchMock.mockResolvedValueOnce(json(counts));
    const got = await governance.getApprovalCounts();
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/approval-counts");
    expect(init.method).toBe("GET");
    expect(got).toEqual(counts);
  });

  it("putApprovalCounts PUTs the body and resolves to the counts", async () => {
    const body: ApprovalCountsWrite = {
      default: { required: 2, require_security: false },
      kinds: [{ kind: "governance_unit", required: 3, require_security: true }],
    };
    fetchMock.mockResolvedValueOnce(json(counts));
    const got = await governance.putApprovalCounts(body);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/approval-counts");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual(body);
    expect(got).toEqual(counts);
  });

  it("putApprovalCounts throws PendingChangeError when the edit is held", async () => {
    const held = { ...change, target_kind: "governance_approval_counts" };
    fetchMock.mockResolvedValueOnce(json({ pending_change: held }, 202));
    const err = await governance
      .putApprovalCounts({ default: { required: 2, require_security: false }, kinds: [] })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PendingChangeError);
    expect((err as PendingChangeError).change.target_kind).toBe("governance_approval_counts");
  });

  it("putApprovalCounts reaches the caller as an HttpError on a refusal", async () => {
    fetchMock.mockResolvedValueOnce(json({ error: "unset", reason: "governance_approval_counts_minimum_unset" }, 409));
    await expect(governance.putApprovalCounts({ default: { required: 2, require_security: false }, kinds: [] })).rejects.toMatchObject({
      status: 409,
    });
  });
});
