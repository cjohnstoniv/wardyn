/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { policies } from "./policies";
import type { RunPolicySpec } from "../types";

// The save doors answer the stored policy plus `requirements` (pkg/client's
// PolicySaved). The client hands the list through untouched for the editor.
describe("policy save client methods", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const spec = { min_confinement_class: "CC2" } as RunPolicySpec;
  const saved = {
    id: "pol-1",
    name: "payments",
    spec,
    requirements: [
      { kind: "secret", name: "stripe-key", status: "missing", fix: "add_secret" },
      { kind: "secret", name: "deploy-pat", status: "present" },
    ],
  };
  const reply = (status: number) =>
    new Response(JSON.stringify(saved), { status, headers: { "Content-Type": "application/json" } });

  it("createPolicy() returns the requirements the server listed", async () => {
    fetchMock.mockResolvedValueOnce(reply(201));
    const res = await policies.createPolicy("payments", spec);
    expect(res.requirements).toEqual(saved.requirements);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/policies");
    expect(init?.method).toBe("POST");
  });

  it("updatePolicy() returns the requirements the server listed", async () => {
    fetchMock.mockResolvedValueOnce(reply(200));
    const res = await policies.updatePolicy("pol-1", "payments", spec);
    expect(res.requirements.map((r) => r.status)).toEqual(["missing", "present"]);
    expect(fetchMock.mock.calls[0][1]?.method).toBe("PUT");
  });
});
