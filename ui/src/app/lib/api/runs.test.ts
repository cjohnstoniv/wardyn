/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runs } from "./runs";

// grantsFromRecords projects the GET /runs/{id}/grants ELIGIBILITY records into
// the CredentialGrant rows the run-detail screen renders. It had zero coverage
// (U073) and its only consumer screen has no test. Reached here through its
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
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        items: [
          {
            id: "g-1",
            created_at: "2026-07-17T00:00:00Z",
            spec: { kind: "github_token", scope: { repo: "acme/widgets" } },
          },
        ],
      }),
    );
    const [g] = await runs.getGrants("run-1");
    expect(g).toMatchObject({
      id: "g-1",
      audience: "github_token",
      state: "active",
      minted_at: "2026-07-17T00:00:00Z",
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
