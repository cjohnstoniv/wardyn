/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runs } from "./runs";

// createRun/preflightRun build their request body from an explicit whitelist
// rather than spreading the input. That is deliberate — it keeps wizard-local
// state out of the wire — but it has silently dropped a real operator choice
// once already (image/task_mode, which made BYOI look like it worked and then
// launch the wrong image). These tests pin the fields the composition model
// added, on BOTH paths, because preflight predicting a launch it doesn't match
// is the same class of lie.
describe("runs api — composition-model fields reach the wire", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ id: "run_1" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const sentBody = () => JSON.parse(String(fetchMock.mock.calls[0][1]?.body ?? "{}"));

  const input = {
    agent: "claude-code",
    repo: "acme/payments",
    task: "do the thing",
    integration_id: "anthropic_api_key",
    workspaces: [
      { workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"], read_only: true },
    ],
  };

  it("createRun forwards workspaces[] and integration_id", async () => {
    await runs.createRun(input);
    const body = sentBody();
    expect(body.integration_id).toBe("anthropic_api_key");
    expect(body.workspaces).toEqual([
      { workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"], read_only: true },
    ]);
  });

  it("preflightRun sends the same two fields, so Review predicts what launch does", async () => {
    await runs.preflightRun(input);
    const body = sentBody();
    expect(body.integration_id).toBe("anthropic_api_key");
    expect(body.workspaces).toHaveLength(1);
    expect(body.workspaces[0].workspace_id).toBe("ws-1");
  });

  it("omits both when the run selects nothing — an absent field is not an empty choice", async () => {
    await runs.createRun({ agent: "claude-code", repo: "acme/payments", task: "t" });
    const body = sentBody();
    expect("workspaces" in body).toBe(false);
    expect("integration_id" in body).toBe(false);
  });
});
