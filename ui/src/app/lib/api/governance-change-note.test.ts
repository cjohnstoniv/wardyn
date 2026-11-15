/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The change-note client: the note PUT and the security-only list filter.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { governance } from "./governance";
import { aheadByHours } from "../test-clock";

const CHANGE = {
  id: "c1",
  target_kind: "governance_unit",
  op: "update",
  target_key: "u1",
  state: "pending",
  proposed_by: "ana@example.com",
  proposed_at: aheadByHours(-1),
  expires_at: aheadByHours(20),
  diff: { changed: [] },
  note: { reason: "why", ticket: "SUP-1" },
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("change note client", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("noteChange PUTs {reason, ticket} to the note route and returns the change", async () => {
    fetchMock.mockResolvedValueOnce(json(CHANGE));
    const got = await governance.noteChange("c1", { reason: "why", ticket: "SUP-1" });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/governance/changes/c1/note");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual({ reason: "why", ticket: "SUP-1" });
    expect(got.note).toEqual({ reason: "why", ticket: "SUP-1" });
  });

  it("noteChange sends both fields empty to clear the note", async () => {
    fetchMock.mockResolvedValueOnce(json({ ...CHANGE, note: undefined }));
    await governance.noteChange("c1", { reason: "", ticket: "" });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ reason: "", ticket: "" });
  });

  it("listChanges({approver: security}) sends the query, and listChanges() sends none", async () => {
    fetchMock.mockResolvedValueOnce(json([CHANGE])).mockResolvedValueOnce(json([CHANGE]));
    await governance.listChanges({ approver: "security" });
    await governance.listChanges();
    expect(fetchMock.mock.calls[0][0]).toContain("/governance/changes?approver=security");
    expect(fetchMock.mock.calls[1][0]).not.toContain("?");
  });

  it("listChanges marks the list truncated only when the server sends X-Wardyn-Truncated", async () => {
    const cut = json([CHANGE]);
    cut.headers.set("X-Wardyn-Truncated", "true");
    fetchMock.mockResolvedValueOnce(cut).mockResolvedValueOnce(json([CHANGE]));
    expect((await governance.listChanges()).truncated).toBe(true);
    expect((await governance.listChanges()).truncated).toBeUndefined();
  });
});
