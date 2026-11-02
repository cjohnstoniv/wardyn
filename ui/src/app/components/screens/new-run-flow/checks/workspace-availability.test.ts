/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { resolvedModelProviders, sourceRunnerLabel, workspaceModelProviderUnavailable, workspaceRunnerRefusal } from "./workspace-availability";
import { folderSources } from "../steps/files/file-items";
import { makeWorkspace } from "../../../../../test/factories";

describe("resolvedModelProviders", () => {
  it("no status at all (not fetched yet) stays undefined", () => {
    expect(resolvedModelProviders(null)).toBeUndefined();
    expect(resolvedModelProviders(undefined)).toBeUndefined();
  });

  it("an unreachable status stays undefined, even carrying a model_providers array", () => {
    expect(resolvedModelProviders({ unreachable: true, model_providers: [] })).toBeUndefined();
  });

  it("a reachable status with no model_providers key stays undefined (no provider block at all)", () => {
    expect(resolvedModelProviders({})).toBeUndefined();
  });

  it("a reachable status with model_providers: [] passes it through (a block exists, granted nothing)", () => {
    expect(resolvedModelProviders({ model_providers: [] })).toEqual([]);
  });

  it("a reachable status with a real list passes it through unchanged", () => {
    const rows = [{ id: "corp-gateway", kind: "anthropic_api_key", harnesses: [], host: "api.anthropic.com" }];
    expect(resolvedModelProviders({ model_providers: rows })).toBe(rows);
  });
});

// The server hides a pin the caller isn't granted and sends llm_cred.provider_unavailable instead.
describe("a pin the server hid (provider_unavailable)", () => {
  const hidden = makeWorkspace({ id: "ws1", llm_cred: { provider_unavailable: true } });
  const visible = makeWorkspace({ id: "ws2", llm_cred: { provider_ref: "corp" } });

  it("is unavailable for an agent run, whatever the caller's list holds", () => {
    expect(workspaceModelProviderUnavailable(hidden, [{ id: "corp" }])).toBe(true);
    expect(workspaceModelProviderUnavailable(hidden, [])).toBe(true);
    expect(workspaceModelProviderUnavailable(visible, [{ id: "corp" }])).toBe(false);
  });

  it("says nothing where the model-provider arm does not apply (no list)", () => {
    expect(workspaceModelProviderUnavailable(hidden, undefined)).toBe(false);
  });
});

// A folder bound to a person's runner (runner_id) stays usable only while the server says the binding is claimed.
describe("a folder bound to a runner", () => {
  // The server projects runner_name while the runner exists; a removed runner has none.
  const bound = (runner_state: "claimed" | "revoked" | "missing" | "not_owner", over = {}) =>
    makeWorkspace({
      id: "ws-r",
      name: "payments",
      sources: [
        {
          type: "local_dir",
          path: "/Users/dana/work/payments",
          runner_id: "abcdef12-3456-7890-abcd-ef1234567890",
          runner_state,
          ...(runner_state === "missing" ? {} : { runner_name: "ec2-mac-rc9" }),
        },
      ],
      ...over,
    });

  it.each([
    ["revoked", "runner revoked"],
    ["missing", "runner removed"],
    ["not_owner", "not your runner"],
  ] as const)("a %s runner blocks the workspace with the named refusal", (state, word) => {
    const ws = bound(state);
    const runner = state === "missing" ? "abcdef12" : "ec2-mac-rc9";
    const refusal = `The folder for payments is on ${runner}, which can't run it now (${word}). Add a new workspace with a folder on a runner you own or on the Wardyn server.`;
    expect(workspaceRunnerRefusal(ws)).toBe(refusal);
    // The run's folder step lists the folder with the same reason, so it cannot be included.
    expect(folderSources([ws])[0].unavailable).toBe(refusal);
  });

  it("names the runner by runner_name, and by the first 8 characters of its id only when there is none", () => {
    const id = "abcdef12-3456-7890-abcd-ef1234567890";
    expect(sourceRunnerLabel({ runner_id: id, runner_name: "ec2-mac-rc9" })).toBe("ec2-mac-rc9");
    expect(sourceRunnerLabel({ runner_id: id, runner_name: "" })).toBe("abcdef12");
    expect(sourceRunnerLabel({ runner_id: id })).toBe("abcdef12");
  });

  it("a claimed runner blocks nothing, and a bound source is never blocked for not being scanned", () => {
    for (const status of ["pending_scan", "scanning", "scanned", "error"] as const) {
      const ws = bound("claimed", { status });
      expect(workspaceRunnerRefusal(ws)).toBeUndefined();
      expect(folderSources([ws])[0].unavailable).toBeUndefined();
    }
  });

  it("a folder on the server has no runner refusal", () => {
    const ws = makeWorkspace({ sources: [{ type: "local_dir", path: "/srv/payments" }] });
    expect(workspaceRunnerRefusal(ws)).toBeUndefined();
  });
});
