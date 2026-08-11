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
    // No status on the frame => 502, the backend-failure fallback.
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toMatchObject({
      status: 502,
      message: "backend exploded",
    });
  });

  it("keeps the status the error frame carries instead of calling every failure a 502", async () => {
    // A post-flush REFUSAL (422 — e.g. an un-onboarded workspace) is not a backend
    // failure: the backend answered, and retrying can never clear it. The stream
    // already sent 200, so the server puts the status it would have returned on the
    // buffer transport ON the frame; flattening it to 502 here made new-run-dialog's
    // composeErrorMessage say "the composer backend failed to respond … try again".
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'data: {"type":"stage","stage":"check"}\n\n',
        'data: {"type":"error","status":422,"error":"workspace: repo \\"acme/payments\\" is not an onboarded repository"}\n\n',
      ]),
    );
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toMatchObject({
      status: 422,
      message: 'workspace: repo "acme/payments" is not an onboarded repository',
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
    // The old per-run subscription opt-in field no longer exists at all (model
    // access now resolves from integrations, never a per-run toggle) — this
    // just guards that its deletion doesn't quietly resurface.
    expect(body).not.toHaveProperty("use_subscription");
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
// onboarded multi-select) into the compose wire shape(s) — mirrors buildSpec's
// per-selection resolution in wizard-types.ts, and (Item 1 fix) shares its
// resolvedMountReadOnly reader instead of a separate `!sel.readOnly`
// derivation, so the two paths can't resolve write access differently for
// the identical picker state. Returns an ARRAY (PARITY-2): a multi-source
// workspace has no single {kind,path/repo} descriptor to flatten to, so one
// selection can resolve to several wire entries — a single-source workspace
// (every fixture below except the dedicated multi-source block) still
// resolves to exactly one, so `[0]` is that one entry. Pure so it's
// unit-testable without mocking fetch.
describe("resolveComposeWorkspace", () => {
  const repoWs: Workspace = {
    id: "ws-repo",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "now",
    updated_at: "now",
  };
  const localWs: Workspace = {
    id: "ws-local",
    name: "app",
    kind: "local_dir",
    source: "/home/me/app",
    status: "scanned",
    created_at: "now",
    updated_at: "now",
  };
  const available = [repoWs, localWs];

  it("resolves a repo selection to kind git + repo source", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-repo" };
    expect(resolveComposeWorkspace(sel, available)).toEqual([{ kind: "git", repo: "acme/payments" }]);
  });

  // Item 1 (HIGH): this used to read `read_write: !sel.readOnly`, which is
  // `true` for BOTH the write toggle's unchecked/undefined state and its
  // checked/false state — so a workspace with no write contract at all
  // (no `requirements`/`effective_requirements`, exactly this fixture) got a
  // silent read-write host mount just because it was attached. The safe
  // baseline (mirrors wizard-types.test.ts's identical "no write requirement
  // declared at all defaults to the safe read-only baseline" case for the
  // manual wizard) is read-only.
  it("resolves a local_dir selection with no write contract to a READ-ONLY mount (safe baseline)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local" };
    expect(resolveComposeWorkspace(sel, available)).toEqual([
      { kind: "local", path: "/home/me/app", read_write: false },
    ]);
  });

  it("honors readOnly: true on a local selection (read_write: false)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local", readOnly: true };
    expect(resolveComposeWorkspace(sel, available)[0]?.read_write).toBe(false);
  });

  // Item 1's exact reported mechanism: source_scan.go seeds an OPTIONAL
  // write:<path> on every scanned local dir, so this — not the bare
  // no-contract fixture above — is the realistic default shape a scanned
  // workspace carries. Both directions of the picker's write toggle:
  describe("an Optional write:<path> requirement (the scan-seeded common case)", () => {
    const wsWithOptionalWrite = {
      ...localWs,
      requirements: { "write:/home/me/app": { level: "optional", provenance: "scan_seeded" } },
    } as Workspace;

    it("toggle OFF (no enabledOptional entry) resolves read-only on the wire", () => {
      const sel: WorkspaceSelection = { workspaceId: "ws-local" };
      expect(resolveComposeWorkspace(sel, [wsWithOptionalWrite])[0]?.read_write).toBe(false);
    });

    it("toggle ON (enabledOptional carries the write key) resolves read-write on the wire", () => {
      // Mirrors workspace-picker.tsx's toggleOptionalWrite, which sets both
      // fields in the SAME patch: enabledOptional gains the key AND readOnly
      // clears to false.
      const sel = {
        workspaceId: "ws-local",
        enabledOptional: ["write:/home/me/app"],
        readOnly: false,
      };
      expect(resolveComposeWorkspace(sel, [wsWithOptionalWrite])[0]?.read_write).toBe(true);
    });

    it("a stray readOnly:false with the key NOT enabled still can't widen an un-granted default", () => {
      const sel = { workspaceId: "ws-local", readOnly: false };
      expect(resolveComposeWorkspace(sel, [wsWithOptionalWrite])[0]?.read_write).toBe(false);
    });
  });

  it("a Required write:<path> resolves read-write with no enabledOptional/readOnly set", () => {
    const wsWithRequiredWrite = {
      ...localWs,
      requirements: { "write:/home/me/app": { level: "required", provenance: "operator_set" } },
    } as Workspace;
    const sel: WorkspaceSelection = { workspaceId: "ws-local" };
    expect(resolveComposeWorkspace(sel, [wsWithRequiredWrite])[0]?.read_write).toBe(true);
  });

  it("returns an empty array for a stale selection (workspace no longer onboarded)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-deleted" };
    expect(resolveComposeWorkspace(sel, available)).toEqual([]);
  });

  // PARITY-2: a multi-source workspace has no single kind/source to flatten
  // to (the server's own deriveWorkspaceMirrors leaves Kind/Source empty for
  // exactly this case) — the old flatten produced a garbage
  // {kind:"local",path:""} entry (or, for a mirror that read "repo", an empty
  // repo string). Iterating .sources fixes it: one entry per repo/local_dir
  // source, nothing for ephemeral.
  describe("a multi-source workspace (PARITY-2)", () => {
    const multiWs = {
      id: "ws-multi",
      name: "monorepo-plus-scratch",
      // The single-mirror fields a real multi-source record leaves EMPTY
      // (internal/store/store.go's deriveWorkspaceMirrors bails on
      // len(Sources) != 1) — asserting the fix does NOT read these.
      kind: "" as unknown as Workspace["kind"],
      source: "",
      status: "scanned",
      created_at: "now",
      updated_at: "now",
      sources: [
        { type: "local_dir", path: "/home/me/api" },
        { type: "repo", source: "acme/widgets" },
        { type: "ephemeral", target: "/home/agent/scratch" },
      ],
      // Drives read_write below via resolvedMountReadOnly (keyed on THIS
      // source's own path, not the whole workspace — PARITY-2) — the
      // source's own (server-only) `writable` field plays no part client-side.
      requirements: { "write:/home/me/api": { level: "required", provenance: "operator_set" } },
    } as Workspace;

    it("emits one entry per repo/local_dir source and nothing for the ephemeral one", () => {
      const sel: WorkspaceSelection = { workspaceId: "ws-multi" };
      expect(resolveComposeWorkspace(sel, [multiWs])).toEqual([
        { kind: "local", path: "/home/me/api", read_write: true },
        { kind: "git", repo: "acme/widgets" },
      ]);
    });

    it("never falls back to an empty single-mirror descriptor", () => {
      const sel: WorkspaceSelection = { workspaceId: "ws-multi" };
      const out = resolveComposeWorkspace(sel, [multiWs]);
      expect(out.some((w) => w.kind === "local" && w.path === "")).toBe(false);
      expect(out.some((w) => w.kind === "git" && w.repo === "")).toBe(false);
    });
  });

  it("a purely ephemeral (migrated legacy container) workspace resolves to an empty array", () => {
    // Migration 0029's exact rewrite: sources=[{type:ephemeral,...}] +
    // base_image={kind:custom,...} — mirrors as Kind="ephemeral", Source="".
    const ephemeralWs = {
      id: "ws-eph",
      name: "old-container",
      kind: "ephemeral" as unknown as Workspace["kind"],
      source: "",
      status: "scanned",
      created_at: "now",
      updated_at: "now",
      sources: [{ type: "ephemeral", target: "/home/agent/work" }],
    } as Workspace;
    const sel: WorkspaceSelection = { workspaceId: "ws-eph" };
    expect(resolveComposeWorkspace(sel, [ephemeralWs])).toEqual([]);
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
    status: "scanned",
    created_at: "now",
    updated_at: "now",
  };

  it("sends the resolved workspaces[] array, with the first selection primary, when selections are given", async () => {
    const localWs: Workspace = {
      id: "ws-local",
      name: "app",
      kind: "local_dir",
      source: "/home/me/app",
      status: "scanned",
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
    // localWs carries no write contract, so it resolves read-only (Item 1's
    // safe baseline) — the point of THIS test is the array shape/ordering
    // (both entries present, local first/primary); write-access semantics are
    // covered exhaustively by the resolveComposeWorkspace describe block above.
    expect(body.workspaces).toEqual([
      { kind: "local", path: "/home/me/app", read_write: false },
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

  // Stage 2: workspaceOptions carries the onboarded id + optional-requirement
  // opt-ins (client.WorkspaceSelection's wire shape) SEPARATELY from the
  // resolved {kind,path,repo} `workspaces` source list above — the server only
  // echoes it back on the proposal (see composeProposed.WorkspaceSelections)
  // so approveLaunch can forward it to createRun without re-deriving it.
  it("sends workspaceOptions verbatim as workspace_selections", async () => {
    await composer.compose(
      {
        prompt: "fix CI",
        workspaceSelections: [{ workspaceId: "ws-repo" }],
        workspaceOptions: [{ workspace_id: "ws-repo", enabled_optional: ["egress:api.stripe.com"] }],
      },
      [repoWs],
    );
    const body = lastBody();
    expect(body.workspace_selections).toEqual([
      { workspace_id: "ws-repo", enabled_optional: ["egress:api.stripe.com"] },
    ]);
  });

  it("omits workspace_selections when workspaceOptions is empty/absent", async () => {
    await composer.compose({ prompt: "fix CI", workspaceSelections: [{ workspaceId: "ws-repo" }] }, [repoWs]);
    const body = lastBody();
    expect(body.workspace_selections).toBeUndefined();
  });
});
