/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { workspaces } from "./workspaces";
import type { Workspace } from "../types";

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
    const res = await workspaces.listWorkspaces();
    expect(res).toEqual([ws]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces");
    expect(init?.method).toBe("GET");
  });

  it("listWorkspaces() yields [] for a null body (a nil Go slice encodes as null)", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response("null", { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    expect(await workspaces.listWorkspaces()).toEqual([]);
  });

  it("createWorkspace() POSTs the input body and returns the created workspace", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(ws), { status: 201, headers: { "Content-Type": "application/json" } }),
    );
    const res = await workspaces.createWorkspace({
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
    const res = await workspaces.updateWorkspace("ws-1", {
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
    await expect(workspaces.deleteWorkspace("ws-1")).resolves.toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1");
    expect(init?.method).toBe("DELETE");
  });

  it("scanWorkspace() POSTs /scan; a 200 (everything scanned inline) reports as sync", async () => {
    // Everything-inline (or ephemeral/dangling) returns the freshly-merged
    // Workspace with 200 (source_scan.go handleScanWorkspace's fall-through)
    // — no scan_run_ids key at all. The client reports the scan as sync.
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ id: "ws-1", status: "ready" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await workspaces.scanWorkspace("ws-1");
    expect(res).toEqual({ async: false, scanRunIds: [] });
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1/scan");
    expect(init?.method).toBe("POST");
  });

  // F6-F4: handleScanWorkspace's 202 body is `{"scan_run_ids": [...], "workspace_id": ...}`
  // — PLURAL, one run per attached repo source (source_scan.go's
  // scanAttachedSources) — never the singular `scan_run_id` a different route
  // (the single-source handleScanSource) answers with. The old stub read the
  // wrong key entirely, so every multi-repo scan silently reported no run ids.
  it("scanWorkspace() reports a governed scan (202, PLURAL scan_run_ids) as async with the ids", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ scan_run_ids: ["run-9", "run-10"], workspace_id: "ws-1" }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await workspaces.scanWorkspace("ws-1");
    expect(res).toEqual({ async: true, scanRunIds: ["run-9", "run-10"] });
  });

  it("scanWorkspace() 202 with no scan_run_ids key (older daemon) reports async with an empty list, never undefined", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ workspace_id: "ws-1" }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const res = await workspaces.scanWorkspace("ws-1");
    expect(res).toEqual({ async: true, scanRunIds: [] });
  });

  it("createWorkspace() accepts the composition shape (sources + base_image) alongside the legacy fields", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(ws), { status: 201, headers: { "Content-Type": "application/json" } }),
    );
    await workspaces.createWorkspace({
      name: "payments",
      sources: [{ type: "local_dir", path: "/home/me/payments", target: "/home/agent/work" }],
      base_image: { kind: "recommended" },
    });
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(String(init?.body))).toEqual({
      name: "payments",
      sources: [{ type: "local_dir", path: "/home/me/payments", target: "/home/agent/work" }],
      base_image: { kind: "recommended" },
    });
  });

  // setRequirements() — PUT /workspaces/{id}/requirements, wrapping the
  // desired map under a "requirements" key (internal/api/workspace_requirements.go's
  // handleSetWorkspaceRequirements decodes `{"requirements": {...}}`, not a
  // bare map) and returning the server's updated workspace.
  it("setRequirements() PUTs the map nested under a `requirements` key and returns the updated workspace", async () => {
    const updated = { ...ws, id: "ws-1" };
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(updated), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const reqs = {
      "secret:acme-key": { level: "required" as const, provenance: "operator_set" as const },
      "egress:api.stripe.com": { level: "optional" as const, provenance: "scan_seeded" as const },
    };
    const res = await workspaces.setRequirements("ws-1", reqs);
    expect(res).toEqual(updated);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/workspaces/ws-1/requirements");
    expect(init?.method).toBe("PUT");
    expect(JSON.parse(String(init?.body))).toEqual({ requirements: reqs });
  });

  // W20-S1-2: a 202 body always carries `warnings` (the masking caveat, at
  // minimum) and the launch's real `confinement_class` — recordTask() must
  // surface both rather than discarding everything but record_run_id.
  it("recordTask() surfaces the 202 body's warnings and confinement_class, not just record_run_id", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          record_run_id: "run-1",
          confinement_class: "CC3",
          warnings: ["masking caveat text"],
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      ),
    );
    const res = await workspaces.recordTask("ws-1", "build & test", false);
    expect(res).toEqual({
      ok: true,
      status: 202,
      record_run_id: "run-1",
      confinement_class: "CC3",
      warnings: ["masking caveat text"],
    });
  });
});
