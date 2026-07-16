/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api, resolveComposeWorkspace } from "./api";
import type { Workspace, WorkspaceSelection } from "./types";

// Regression for the sign-out HIGH finding: signing out only cleared the local
// admin token, never calling the server logout endpoint, so the OIDC session
// cookie survived and the next auth probe silently re-signed-in. api.logout()
// must hit /api/v1/auth/logout with the cookie attached (credentials:include),
// so the server can clear the session.
describe("api.logout()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("calls the auth/logout endpoint with credentials included", async () => {
    await api.logout();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/auth/logout");
    expect(init?.credentials).toBe("include");
  });

  it("resolves even when the server returns an error (best-effort logout)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("boom", { status: 500 }));
    await expect(api.logout()).resolves.toBeUndefined();
  });

  it("resolves even when the network throws (best-effort logout)", async () => {
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    await expect(api.logout()).resolves.toBeUndefined();
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
  const workspaces = [repoWs, localWs];

  it("resolves a repo selection to kind git + repo source", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-repo" };
    expect(resolveComposeWorkspace(sel, workspaces)).toEqual({
      kind: "git",
      repo: "acme/payments",
    });
  });

  it("resolves a local_dir selection to kind local + path source, read_write true by default", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local" };
    expect(resolveComposeWorkspace(sel, workspaces)).toEqual({
      kind: "local",
      path: "/home/me/app",
      read_write: true,
    });
  });

  it("honors readOnly: true on a local selection (read_write: false)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-local", readOnly: true };
    expect(resolveComposeWorkspace(sel, workspaces)?.read_write).toBe(false);
  });

  it("returns undefined for a stale selection (workspace no longer onboarded)", () => {
    const sel: WorkspaceSelection = { workspaceId: "ws-deleted" };
    expect(resolveComposeWorkspace(sel, workspaces)).toBeUndefined();
  });
});

// api.compose() wire-shape: the onboarded multi-select wins when present (sends
// `workspaces[]`, no legacy `workspace`); an empty/absent selection falls back
// to the legacy singular `workspace` (ephemeral by default). See
// internal/api/compose.go's ComposeRequest.Workspaces.
describe("api.compose() — workspaces[] wire shape", () => {
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

  afterEach(() => {
    vi.unstubAllGlobals();
  });

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
    await api.compose(
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
    await api.compose({ prompt: "fix CI", workspaceSelections: [] }, [repoWs]);
    const body = lastBody();
    expect(body.workspace).toEqual({ kind: "ephemeral" });
    expect(body.workspaces).toBeUndefined();
  });
});

// Server-error passthrough (ui-arch): these write/delete methods used to throw a
// hardcoded generic string and DISCARD the server's real {"error":"..."} body, so
// an actionable reason (a 409 conflict, an FK-constraint delete reason) never
// reached the operator. They now thread it through via errText(), the same helper
// verifyWorkspace()/recordTask() already use.
describe("server-error passthrough (write/delete endpoints)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function errorResponse(status: number, message: string): Response {
    return new Response(JSON.stringify({ error: message }), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  }

  it("killRun() surfaces the server's actual reason, not a generic string", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "run is already terminal"));
    await expect(api.killRun("run-1")).rejects.toMatchObject({ message: "run is already terminal" });
  });

  it("deletePolicy() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "policy is in use by 2 runs"));
    await expect(api.deletePolicy("pol-1")).rejects.toMatchObject({
      message: "policy is in use by 2 runs",
    });
  });

  it("setSecret() surfaces exactly the server's parsed error, not the raw JSON body", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(400, "value must not be empty"));
    await expect(api.setSecret("anthropic-api-key", "")).rejects.toMatchObject({
      message: "value must not be empty",
    });
  });

  it("deleteSecret() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "secret is referenced by an active policy"));
    await expect(api.deleteSecret("anthropic-api-key")).rejects.toMatchObject({
      message: "secret is referenced by an active policy",
    });
  });

  it("deleteWorkspace() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "workspace has an active run"));
    await expect(api.deleteWorkspace("ws-1")).rejects.toMatchObject({
      message: "workspace has an active run",
    });
  });
});
