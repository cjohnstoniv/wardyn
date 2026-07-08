/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api, HttpError } from "./api";
import type { ComposeResponse, ComposerBackend } from "./types";

// Unit tests for the AI Run Composer API client methods. They pin the request
// shapes (path, method, JSON body) and the response/error mapping against the
// compose wire contract in internal/api/compose.go.
describe("api.compose() + api.listComposerBackends()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

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
    const res = await api.compose({
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
    await api.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
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
    await api.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      useSubscription: true,
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.use_subscription).toBe(true);
  });

  it("threads the persisted default tier as confinement_floor when set", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await api.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      confinementFloor: "CC3",
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.confinement_floor).toBe("CC3");
  });

  it("omits confinement_floor entirely when no default tier is persisted", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await api.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    // Absent (not "") so an old server never sees an unknown field.
    expect(body).not.toHaveProperty("confinement_floor");
  });

  // Decision 1/9: the client mints and owns the compose-session id — the server
  // holds no session state, so it must be resent unchanged on every round.
  it("threads the client-owned session id as session_id when set", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await api.compose({
      prompt: "build the site",
      workspace: { kind: "ephemeral" },
      sessionId: "11111111-1111-1111-1111-111111111111",
    });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body.session_id).toBe("11111111-1111-1111-1111-111111111111");
  });

  it("omits session_id entirely before a session has been minted", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(sampleResponse));
    await api.compose({ prompt: "just a prompt", workspace: { kind: "ephemeral" } });
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
    const res = await api.compose({ prompt: "x", workspace: { kind: "ephemeral" } });
    if (res.kind !== "proposal") throw new Error("expected a proposal response");
    expect(res.setup_items).toHaveLength(1);
    expect(res.setup_items?.[0]).toMatchObject({ kind: "llm_access", status: "missing" });
  });

  it("throws an HttpError carrying the status on a 404 (composer disabled)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("disabled", { status: 404 }));
    await expect(api.compose({ prompt: "x", workspace: { kind: "ephemeral" } })).rejects.toMatchObject({ status: 404 });
  });

  it("throws an HttpError carrying the status on a 413 (too large)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("too big", { status: 413 }));
    const err = await api.compose({ prompt: "x", workspace: { kind: "ephemeral" } }).catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(413);
  });

  it("throws an HttpError carrying the status on a 502 (backend failure)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("backend down", { status: 502 }));
    await expect(api.compose({ prompt: "x", workspace: { kind: "ephemeral" } })).rejects.toMatchObject({ status: 502 });
  });

  it("listComposerBackends returns the backends array in wire shape", async () => {
    const backends: ComposerBackend[] = [
      { name: "anthropic-default", provider: "anthropic", model: "claude", is_default: true },
      { name: "openai", provider: "openai", model: "gpt", is_default: false },
    ];
    fetchMock.mockResolvedValueOnce(jsonResponse({ backends }));
    const out = await api.listComposerBackends();

    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/composer/backends");
    expect(init?.method).toBe("GET");
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({ name: "anthropic-default", is_default: true });
  });

  it("listComposerBackends returns [] when the composer is disabled (404)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("disabled", { status: 404 }));
    await expect(api.listComposerBackends()).resolves.toEqual([]);
  });

  it("listComposerBackends tolerates a missing backends key", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({}));
    await expect(api.listComposerBackends()).resolves.toEqual([]);
  });
});

// api.createRun() gains an optional compose_session_id — correlates the launched
// run's audit row back to the compose conversation that produced it.
describe("api.createRun() — compose_session_id", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  }

  it("sends compose_session_id when the run was launched from a compose session", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ id: "run-1" }));
    await api.createRun({
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
    await api.createRun({ agent: "claude-code", repo: "acme/payments", task: "fix CI" });
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body).not.toHaveProperty("compose_session_id");
  });
});
