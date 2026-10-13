/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { resolvedModelProviders, workspaceModelProviderUnavailable } from "./workspace-availability";
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
