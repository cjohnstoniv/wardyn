/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api } from "./api";
import type { Workspace } from "./types";

// Workspace CRUD + scan client — mirrors the secrets/policies client methods
// (listX/createX/updateX/deleteX + unwrapList). Only the wire shape and paths
// are worth pinning here; the screens exercise the rest.
describe("workspace client methods", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const ws: Workspace = {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/home/me/payments",
    status: "pending_scan",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };

  it("listWorkspaces() GETs /workspaces and unwraps a bare array", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify([ws]), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await api.listWorkspaces();
    expect(res).toEqual([ws]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces");
    expect(init?.method).toBe("GET");
  });

  it("listWorkspaces() unwraps an { items: [...] } envelope too", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ items: [ws] }), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await api.listWorkspaces();
    expect(res).toEqual([ws]);
  });

  it("createWorkspace() POSTs the input body and returns the created workspace", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(ws), { status: 201, headers: { "Content-Type": "application/json" } }),
    );
    const res = await api.createWorkspace({
      name: "payments",
      kind: "local_dir",
      source: "/home/me/payments",
    });
    expect(res).toEqual(ws);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      name: "payments",
      kind: "local_dir",
      source: "/home/me/payments",
    });
  });

  it("updateWorkspace() PUTs to /workspaces/{id}", async () => {
    const updated = { ...ws, ref: "main" };
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(updated), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await api.updateWorkspace("ws-1", {
      name: "payments",
      kind: "local_dir",
      source: "/home/me/payments",
      ref: "main",
    });
    expect(res).toEqual(updated);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1");
    expect(init?.method).toBe("PUT");
  });

  it("deleteWorkspace() DELETEs /workspaces/{id} and tolerates a 404", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    await expect(api.deleteWorkspace("ws-1")).resolves.toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1");
    expect(init?.method).toBe("DELETE");
  });

  it("scanWorkspace() POSTs /scan; a 200 (local dir, body is the profile) reports as sync", async () => {
    // Local-dir scan returns the derived PROFILE (not a Workspace) with 200 — the
    // client must not treat it as a Workspace; it reports the scan as sync.
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ languages: ["go"] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await api.scanWorkspace("ws-1");
    expect(res).toEqual({ async: false, scanRunId: undefined });
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1/scan");
    expect(init?.method).toBe("POST");
  });

  it("scanWorkspace() reports a repo scan (202 scan-run stub) as async with its scan_run_id", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({ scan_run_id: "run-9", workspace_id: "ws-1", state: "queued" }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      ),
    );
    const res = await api.scanWorkspace("ws-1");
    expect(res).toEqual({ async: true, scanRunId: "run-9" });
  });
});
