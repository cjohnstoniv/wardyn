/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { composer, resolveComposeWorkspace } from "./compose";
import { HttpError } from "./core";
import type { ComposeResponse, ComposerBackend, Workspace, WorkspaceSelection } from "../types";

// The compose() SSE-streaming branch (taken whenever an onStage callback is
// passed) hand-parses `data: <json>\n\n` frames off a ReadableStream. The
// existing composer tests pass no onStage, so the whole streaming path — frame
// buffering, stage dispatch, terminal result/error frames — was unexercised
//. These drive it over a stubbed reader, including a frame split across
// two chunks (the buffering edge the \n\n scan exists for).
describe("composer.compose() — SSE streaming path", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const enc = new TextEncoder();

  // A minimal Response-like object exposing exactly what the streaming branch
  // reads: status/ok (past wfetch's 401 guard), a text/event-stream content-type,
  // and a body.getReader() yielding the given raw chunks in order.
  function sseResponse(chunks: string[]) {
    let i = 0;
    return {
      status: 200,
      ok: true,
      headers: { get: (h: string) => (h === "Content-Type" ? "text/event-stream" : null) },
      body: {
        getReader() {
          return {
            read: async () =>
              i < chunks.length
                ? { value: enc.encode(chunks[i++]), done: false }
                : { value: undefined, done: true },
          };
        },
      },
    };
  }

  const result = {
    kind: "proposal",
    proposed: { run: {}, inline_policy: {} },
    overall_risk: "medium",
    summary: "ok",
  };

  it("dispatches every stage frame to onStage and returns the terminal result", async () => {
    // The propose frame is split across two chunks to exercise the \n\n buffer scan.
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'data: {"type":"stage","stage":"validate"}\n\n',
        'data: {"type":"stage","stage":"pro',
        'pose"}\n\n' + `data: ${JSON.stringify({ type: "result", result })}\n\n`,
      ]),
    );

    const stages: string[] = [];
    const out = await composer.compose({ prompt: "fix CI" }, [], (s) => stages.push(s));

    expect(stages).toEqual(["validate", "propose"]);
    expect(out).toEqual(result);
    // Streaming was requested via the Accept header.
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(new Headers(init.headers).get("Accept")).toBe("text/event-stream");
  });

  it("throws an HttpError when the stream carries an error frame", async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'data: {"type":"stage","stage":"detect"}\n\n',
        'data: {"type":"error","error":"backend exploded"}\n\n',
      ]),
    );
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toMatchObject({
      status: 502,
      message: "backend exploded",
    });
  });

  it("throws when the stream ends without a result frame", async () => {
    fetchMock.mockResolvedValueOnce(sseResponse(['data: {"type":"stage","stage":"validate"}\n\n']));
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toBeInstanceOf(HttpError);
  });

  it("falls back to the JSON path when the server does not stream", async () => {
    // A non-streaming server (or a pre-flush 4xx) returns plain JSON even though
    // onStage was passed — compose() must parse it via the synchronous path.
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(result), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const stages: string[] = [];
    const out = await composer.compose({ prompt: "x" }, [], (s) => stages.push(s));
    expect(stages).toEqual([]);
    expect(out).toEqual(result);
  });
});

// Request shapes (path, method, JSON body) and response/error mapping for the
// non-streaming path, pinned against the compose wire contract in
// internal/api/compose.go.
describe("composer.compose() + composer.listComposerBackends()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  }

  const sampleResponse: ComposeResponse = {
    kind: "proposal",
    proposed: {
      run: {
        agent: "claude-code",
        repo: "acme/payments",
        task: "fix the flaky test",
        confinement_class: "CC2",
        interactive: false,
      },
      inline_policy: {
        allowed_domains: ["api.anthropic.com"],
        first_use_approval: "deny_with_review",
        min_confinement_class: "CC2",
      },
    },
    risk_assessment: [
      {
        field: "min_confinement_class",
        value: "CC2",
        risk_level: "medium",
        rationale: "gVisor sandbox.",
      },
    ],
    overall_risk: "medium",
    summary: "A confined batch run.",
    warnings: ["clamped allowed_domains to operator ceiling"],
  };

  it("POSTs /runs/compose with the request body and returns the parsed response", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    const res = await composer.compose({
      prompt: "fix CI",
      workspace: { kind: "git", repo: "acme/widgets" },
      attachments: [{ name: "log.txt", content: "boom" }],
      sources: ["https://example.com/issue/1"],
      backend: "anthropic-default",
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/runs/compose");
    expect(init?.method).toBe("POST");
    const body = JSON.parse(String(init?.body));
    expect(body).toEqual({
      prompt: "fix CI",
      workspace: { kind: "git", repo: "acme/widgets" },
      attachments: [{ name: "log.txt", content: "boom" }],
      sources: ["https://example.com/issue/1"],
      backend: "anthropic-default",
      // run mode is ALWAYS sent (false = background is a real choice, not a default).
      interactive: false,
    });
    // The response is returned in the wire shape (risk_level, overall_risk, etc.).
    if (res.kind !== "proposal") throw new Error("expected a proposal response");
    expect(res.overall_risk).toBe("medium");
    expect(res.proposed.run.agent).toBe("claude-code");
    expect(res.risk_assessment[0].risk_level).toBe("medium");
  });

  it("omits empty optional fields from the compose body", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    // workspace is REQUIRED and always sent; the truly-optional fields are omitted.
    expect(body).toEqual({ prompt: "just a prompt", workspace: { kind: "ephemeral" }, interactive: false });
    expect(body).not.toHaveProperty("attachments");
    expect(body).not.toHaveProperty("sources");
    expect(body).not.toHaveProperty("backend");
    // The subscription opt-in is sent ONLY when ticked (absent = api-key default),
    // so an old server never sees an unknown field on a default request.
    expect(body).not.toHaveProperty("use_subscription");
  });

  it("threads the per-run subscription opt-in as use_subscription when ticked", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      useSubscription: true,
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.use_subscription).toBe(true);
  });

  it("threads the persisted default tier as confinement_floor when set", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      confinementFloor: "CC3",
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.confinement_floor).toBe("CC3");
  });

  it("omits confinement_floor entirely when no default tier is persisted", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    // Absent (not "") so an old server never sees an unknown field.
    expect(body).not.toHaveProperty("confinement_floor");
  });

  // Decision 1/9: the client mints and owns the compose-session id — the server
  // holds no session state, so it must be resent unchanged on every round.
  it("threads the client-owned session id as session_id when set", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      sessionId: "11111111-1111-1111-1111-111111111111",
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.session_id).toBe("11111111-1111-1111-1111-111111111111");
  });

  it("omits session_id entirely before a session has been minted", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await composer.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body).not.toHaveProperty("session_id");
  });

  // setup_items is a NEW optional response field (compose_setup.go's SetupItem[]) —
  // an older server that predates it must still parse fine (the field is simply
  // absent, never a parse error).
  it("returns setup_items when the server includes it in the proposal", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        ...sampleResponse,
        setup_items: [
          {
            id: "secret:anthropic-api-key",
            kind: "llm_access",
            label: "Model access",
            required_by: "claude-code",
            status: "missing",
            fix: { action: "add_secret", secret_name: "anthropic-api-key" },
          },
        ],
      }),
    );
    const res = await composer.compose({ prompt: "x", workspace: { kind: "ephemeral" } });
    if (res.kind !== "proposal") throw new Error("expected a proposal response");
    expect(res.setup_items).toHaveLength(1);
    expect(res.setup_items?.[0]).toMatchObject({ kind: "llm_access", status: "missing" });
  });

  it("throws an HttpError carrying the status on a 404 (composer disabled)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("disabled", { status: 404 }));
    await expect(composer.compose({ prompt: "x", workspace: { kind: "ephemeral" } })).rejects.toMatchObject({
      status: 404,
    });
  });

  it("throws an HttpError carrying the status on a 413 (too large)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("too big", { status: 413 }));
    const err = await composer.compose({ prompt: "x", workspace: { kind: "ephemeral" } }).catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(413);
  });

  it("throws an HttpError carrying the status on a 502 (backend failure)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("backend down", { status: 502 }));
    await expect(composer.compose({ prompt: "x", workspace: { kind: "ephemeral" } })).rejects.toMatchObject({
      status: 502,
    });
  });

  it("listComposerBackends returns the backends array in wire shape", async () => {
    const backends: ComposerBackend[] = [
      { name: "anthropic-default", provider: "anthropic", model: "claude", is_default: true },
      { name: "openai", provider: "openai", model: "gpt", is_default: false },
    ];
    fetchMock.mockResolvedValueOnce(jsonResponse({ backends }));
    const out = await composer.listComposerBackends();

    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/composer/backends");
    expect(init?.method).toBe("GET");
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({ name: "anthropic-default", is_default: true });
  });

  it("listComposerBackends returns [] when the composer is disabled (404)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("disabled", { status: 404 }));
    await expect(composer.listComposerBackends()).resolves.toEqual([]);
  });

  it("listComposerBackends tolerates a missing backends key", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({}));
    await expect(composer.listComposerBackends()).resolves.toEqual([]);
  });

  // a non-404 failure must surface the control plane's `{"error":"…"}`
  // message, not a hardcoded "failed to list composer backends" string that
  // discards the server's actionable reason.
  it("listComposerBackends surfaces the server error message on a 500", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ error: "composer backend registry unavailable" }, 500));
    const err = await composer.listComposerBackends().catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(500);
    expect((err as HttpError).message).toBe("composer backend registry unavailable");
  });
});

// resolveComposeWorkspace resolves a compose-form WorkspaceSelection (from the
// onboarded multi-select) into the compose wire shape — mirrors buildSpec's
// per-selection resolution in wizard-types.ts. Pure so it's unit-testable
// without mocking fetch.
describe("resolveComposeWorkspace", () => {
  const repoWs: Workspace = {
    id: "ws-repo",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "ready",
    created_at: "now",
    updated_at: "now",
  };
  const localWs: Workspace = {
    id: "ws-local",
    name: "app",
    kind: "local_dir",
    source: "/home/me/app",
    status: "ready",
    created_at: "now",
    updated_at: "now",
  };
  const available = [repoWs, localWs];

  it("resolves a repo selection to kind git + repo source", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-repo" };
    expect(resolveComposeWorkspace(sel, available)).toEqual({ kind: "git", repo: "acme/payments" });
  });

  it("resolves a local_dir selection to kind local + path source, read_write true by default", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local" };
    expect(resolveComposeWorkspace(sel, available)).toEqual({
      kind: "local",
      path: "/home/me/app",
      read_write: true,
    });
  });

  it("honors readOnly: true on a local selection (read_write: false)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local", readOnly: true };
    expect(resolveComposeWorkspace(sel, available)?.read_write).toBe(false);
  });

  it("returns undefined for a stale selection (workspace no longer onboarded)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-deleted" };
    expect(resolveComposeWorkspace(sel, available)).toBeUndefined();
  });
});

// The onboarded multi-select wins when present (sends `workspaces[]`, no legacy
// `workspace`); an empty/absent selection falls back to the legacy singular
// `workspace` (ephemeral by default). See internal/api/compose.go's
// ComposeRequest.Workspaces.
describe("composer.compose() — workspaces[] wire shape", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ kind: "proposal" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  function lastBody(): Record<string, unknown> {
    const [, init] = fetchMock.mock.calls.at(-1)!;
    return JSON.parse(init.body as string);
  }

  const repoWs: Workspace = {
    id: "ws-repo",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "ready",
    created_at: "now",
    updated_at: "now",
  };

  it("sends the resolved workspaces[] array, with the first selection primary, when selections are given", async () => {
    const localWs: Workspace = {
      id: "ws-local",
      name: "app",
      kind: "local_dir",
      source: "/home/me/app",
      status: "ready",
      created_at: "now",
      updated_at: "now",
    };
    await composer.compose(
      {
        prompt: "fix CI",
        workspaceSelections: [{ workspaceId: "ws-local" }, { workspaceId: "ws-repo" }],
      },
      [repoWs, localWs],
    );
    const body = lastBody();
    expect(body.workspaces).toEqual([
      { kind: "local", path: "/home/me/app", read_write: true },
      { kind: "git", repo: "acme/payments" },
    ]);
    expect(body.workspace).toBeUndefined();
  });

  it("falls back to the legacy singular ephemeral workspace when there are no selections", async () => {
    await composer.compose({ prompt: "fix CI", workspaceSelections: [] }, [repoWs]);
    const body = lastBody();
    expect(body.workspace).toEqual({ kind: "ephemeral" });
    expect(body.workspaces).toBeUndefined();
  });
});
