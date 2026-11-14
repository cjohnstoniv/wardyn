/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { isKillStartedRefusal } from "./kill-started";
import { policies } from "./policies";
import { runs } from "./runs";
import { secrets } from "./secrets";
import { workspaces } from "./workspaces";

// Server-error passthrough (ui-arch): these write/delete methods used to throw a
// hardcoded generic string and DISCARD the server's real {"error":"..."} body, so
// an actionable reason (a 409 conflict, an FK-constraint delete reason) never
// reached the operator. They now thread it through via errText(), the same helper
// verifyWorkspace()/recordTask() already use. One suite, since the property is a
// cross-domain contract rather than anything specific to one client module.
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
    await expect(runs.killRun("run-1")).rejects.toMatchObject({ message: "run is already terminal" });
  });

  it("killRun() with notStarted asks ?not_started=true and keeps the refusal's reason; without it the URL is unchanged", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: "run has started", reason: "run_kill_started" }), { status: 409, headers: { "Content-Type": "application/json" } }),
    );
    const refused = await runs.killRun("run-1", { notStarted: true }).catch((e: unknown) => e);
    expect(isKillStartedRefusal(refused)).toBe(true);
    expect(String(fetchMock.mock.calls[0][0])).toMatch(/\/runs\/run-1\/kill\?not_started=true$/);
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 202 }));
    await runs.killRun("run-1");
    expect(String(fetchMock.mock.calls[1][0])).toMatch(/\/runs\/run-1\/kill$/);
  });

  it("deletePolicy() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "policy is in use by 2 runs"));
    await expect(policies.deletePolicy("pol-1")).rejects.toMatchObject({
      message: "policy is in use by 2 runs",
    });
  });

  it("setSecret() surfaces exactly the server's parsed error, not the raw JSON body", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(400, "value must not be empty"));
    await expect(secrets.setSecret("anthropic-api-key", "")).rejects.toMatchObject({
      message: "value must not be empty",
    });
  });

  it("deleteSecret() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "secret is referenced by an active policy"));
    await expect(secrets.deleteSecret("anthropic-api-key")).rejects.toMatchObject({
      message: "secret is referenced by an active policy",
    });
  });

  it("deleteWorkspace() surfaces the server's actual reason on a non-404 failure", async () => {
    fetchMock.mockResolvedValueOnce(errorResponse(409, "workspace has an active run"));
    await expect(workspaces.deleteWorkspace("ws-1")).rejects.toMatchObject({
      message: "workspace has an active run",
    });
  });
});
