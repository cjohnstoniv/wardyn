/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runs } from "./runs";

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
    fetchMock.mockResolvedValueOnce(
      jsonResponse([
        {
          id: "g-1",
          created_at: "2026-07-17T00:00:00Z",
          spec: { kind: "github_token", scope: { repo: "acme/widgets" } },
        },
      ]),
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

// runs.createRun() carries an optional compose_session_id — correlates the
// launched run's audit row back to the compose conversation that produced it.
describe("runs.createRun() — compose_session_id", () => {
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

  it("sends compose_session_id when the run was launched from a compose session", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ id: "run-1" }));
    await runs.createRun({
      agent: "claude-code",
      repo: "acme/payments",
      task: "fix CI",
      compose_session_id: "11111111-1111-1111-1111-111111111111",
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.compose_session_id).toBe("11111111-1111-1111-1111-111111111111");
  });

  it("omits compose_session_id for a run with no compose session (e.g. the manual wizard)", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ id: "run-1" }));
    await runs.createRun({ agent: "claude-code", repo: "acme/payments", task: "fix CI" });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body).not.toHaveProperty("compose_session_id");
  });
});
