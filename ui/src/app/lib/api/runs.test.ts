/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runs } from "./runs";
import { aheadByHours } from "../test-clock";

// grantsFromRecords projects the GET /runs/{id}/grants ELIGIBILITY records into
// the CredentialGrant rows the run-detail screen renders. It had zero coverage
// and its only consumer screen has no test. Reached here through its
// export path, runs.getGrants, with a stubbed fetch.
describe("runs.getGrants — grant-record projection", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const jsonResponse = (body: unknown) =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });

  it("compacts a grant with a scope object and reports it active", async () => {
    const mintedAt = aheadByHours(-1);
    fetchMock.mockResolvedValueOnce(
      jsonResponse([
        {
          id: "g-1",
          created_at: mintedAt,
          spec: { kind: "github_token", scope: { repo: "acme/widgets" } },
        },
      ]),
    );
    const [g] = await runs.getGrants("run-1");
    expect(g).toMatchObject({
      id: "g-1",
      audience: "github_token",
      state: "active",
      minted_at: mintedAt,
    });
    expect(g.scope).toBe('github_token {"repo":"acme/widgets"}');
  });

  it("falls back to the kind alone when no scope object is present", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse([{ id: "g-2", spec: { kind: "cloud_sts" } }]));
    const [g] = await runs.getGrants("run-2");
    expect(g.scope).toBe("cloud_sts");
    expect(g.audience).toBe("cloud_sts");
  });

  it("degrades missing id/kind to an em-dash rather than undefined", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse([{ spec: {} }]));
    const [g] = await runs.getGrants("run-3");
    expect(g.id).toBe("—");
    expect(g.scope).toBe("—");
  });

  it("returns an empty list when the payload is not a list", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ nope: true }));
    expect(await runs.getGrants("run-4")).toEqual([]);
  });
});

// R4-F077: listRuns() backs the Runs board's 3s poll AND the Recordings
// screen. The server charges one RecordingStore.StatAndTail call per run for
// ?include=recording_meta, so it must be opt-in — a caller that doesn't ask
// for it (the board) must not send the query key at all.
describe("runs.listRuns — recording-meta opt-in", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(
      new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("omits include= by default", async () => {
    await runs.listRuns();
    expect(String(fetchMock.mock.calls[0][0])).not.toContain("include=");
  });

  it("sends include=recording_meta when asked", async () => {
    await runs.listRuns({ includeRecordingMeta: true });
    expect(String(fetchMock.mock.calls[0][0])).toContain("include=recording_meta");
  });
});

// #159: opting into BOTH limit and offset switches listRuns() onto explicit
// server-side paging and widens its return to { runs, truncated } — reading
// X-Wardyn-Truncated off the response, which unwrapList's bare array return
// could never carry. A caller that omits either (every caller above) keeps
// getting the plain array, unchanged.
describe("runs.listRuns — explicit paging (#159)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const jsonResponse = (body: unknown, headers: Record<string, string> = {}) =>
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json", ...headers } });

  it("sends the explicit ?limit=&offset= instead of the default page", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse([]));
    await runs.listRuns({ limit: 100, offset: 200 });
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain("limit=100");
    expect(url).toContain("offset=200");
  });

  it("reports truncated:true from X-Wardyn-Truncated and returns the rows under .runs", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse([{ id: "run-1" }], { "X-Wardyn-Truncated": "true" }));
    const got = await runs.listRuns({ limit: 100, offset: 0 });
    expect(got.truncated).toBe(true);
    expect(got.runs).toHaveLength(1);
  });

  it("reports truncated:false when the header is absent", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse([]));
    const got = await runs.listRuns({ limit: 100, offset: 0 });
    expect(got.truncated).toBe(false);
  });
});
