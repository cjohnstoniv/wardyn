/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runs } from "./runs";
import { setExpectedPrincipal } from "./core";
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

  it("carries the scope's host (git_pat, ssh_key) and nothing else of the scope onto the host field", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse([{ id: "g-9", spec: { kind: "git_pat", scope: { host: "dev.azure.com", secret: "S" } } }]),
    );
    const [g] = await runs.getGrants("run-9");
    expect(g.host).toBe("dev.azure.com");
    fetchMock.mockResolvedValueOnce(jsonResponse([{ id: "g-8", spec: { kind: "github_token", scope: { repo: "a/b" } } }]));
    expect((await runs.getGrants("run-8"))[0].host).toBeUndefined();
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

describe("runs.listRunsFiltered — sponsored", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } })));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("sends sponsored=exclude only when asked (#2513)", async () => {
    await runs.listRunsFiltered({ view: "user" });
    expect(String(fetchMock.mock.calls[0][0])).not.toContain("sponsored=");
    await runs.listRunsFiltered({ view: "user", sponsored: "exclude" });
    expect(String(fetchMock.mock.calls[1][0])).toContain("sponsored=exclude");
  });
});

describe("runs.listRunsFiltered — service", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } })));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("sends service=1 only when asked", async () => {
    await runs.listRunsFiltered({ view: "user" });
    expect(String(fetchMock.mock.calls[0][0])).not.toContain("service=");
    await runs.listRunsFiltered({ view: "user", service: true });
    expect(String(fetchMock.mock.calls[1][0])).toContain("service=1");
  });
});

describe("runs.listRunsFiltered — counts", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } })));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("skipCounts sends counts=skip and reads countsRead from the headers", async () => {
    await runs.listRunsFiltered({ view: "user" });
    expect(String(fetchMock.mock.calls[0][0])).not.toContain("counts=");
    const skipped = await runs.listRunsFiltered({ view: "user", skipCounts: true });
    expect(String(fetchMock.mock.calls[1][0])).toContain("counts=skip");
    expect(skipped.countsRead).toBe(false);

    fetchMock.mockResolvedValueOnce(
      new Response("[]", {
        status: 200,
        headers: { "Content-Type": "application/json", "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      }),
    );
    const counted = await runs.listRunsFiltered({ view: "user" });
    expect(counted.countsRead).toBe(true);
  });

  it("endedAfter sends ended_after and reads X-Wardyn-Ended-After back", async () => {
    const cut = new Date(Date.now() - 3_600_000).toISOString();
    fetchMock.mockResolvedValueOnce(
      new Response("[]", { status: 200, headers: { "Content-Type": "application/json", "X-Wardyn-Ended-After": cut } }),
    );
    const delta = await runs.listRunsFiltered({ view: "user", skipCounts: true, endedAfter: cut });
    expect(String(fetchMock.mock.calls[0][0])).toContain(`ended_after=${encodeURIComponent(cut)}`);
    expect(delta.endedAfter).toBe(cut);

    const whole = await runs.listRunsFiltered({ view: "user" });
    expect(String(fetchMock.mock.calls[1][0])).not.toContain("ended_after=");
    expect(whole.endedAfter).toBeUndefined();
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

// Review nit: takeoverAttach threw a bare HttpError, so a run_owner_only refusal
// lost its reason and the console could not map it to its own sentence.
describe("runs.takeoverAttach — the refusal keeps its reason", () => {
  afterEach(() => vi.unstubAllGlobals());
  it("carries the envelope's reason on a 403", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: "only the person who started this run can open it interactively", reason: "run_owner_only" }), {
          status: 403,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    await expect(runs.takeoverAttach("run-1")).rejects.toMatchObject({ status: 403, reason: "run_owner_only" });
  });
});

describe("runs.reviveRun — denied_added", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("reads a null denied_added as an empty list", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ run_id: "run-1", denied_added: null, proxy_release: "r1" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    expect((await runs.reviveRun("run-1")).denied_added).toEqual([]);
  });
});

describe("runs.createRunWire — the optional headers", () => {
  afterEach(() => vi.unstubAllGlobals());
  const stub = () => {
    const f = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "run-1" }), { status: 201, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", f);
    return f;
  };
  const headersOf = (f: ReturnType<typeof stub>) => new Headers((f.mock.calls[0] as unknown as [string, RequestInit])[1].headers);

  it("sends no assistant header when none is given", async () => {
    const f = stub();
    await runs.createRunWire({ title: "x" });
    expect(headersOf(f).has("X-Wardyn-Assistant-Session")).toBe(false);
  });

  it("passes a header into the request and still names the person", async () => {
    const f = stub();
    setExpectedPrincipal("alice@example.com");
    try {
      await runs.createRunWire({ title: "x" }, { "X-Wardyn-Assistant-Session": "s-1" });
    } finally {
      setExpectedPrincipal("");
    }
    const h = headersOf(f);
    expect(h.get("X-Wardyn-Assistant-Session")).toBe("s-1");
    // The person marking (asPerson) goes out with the draft, so another tab's cookie cannot submit it as someone else.
    expect(h.get("X-Wardyn-Expected-Principal")).toBe(encodeURIComponent("alice@example.com"));
  });
});
