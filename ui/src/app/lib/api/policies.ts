/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run policies (admin-gated config): list/create/update/delete.
import type { RunPolicy, RunPolicySpec } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch, withLimit } from "./core";

export const policies = {
  // GET /api/v1/policies — all run policies (reverse creation order).
  async listPolicies(): Promise<RunPolicy[]> {
    const res = await wfetch(withLimit("/policies"), { method: "GET" });
    return unwrapList<RunPolicy>(await asJson<unknown>(res));
  },

  // POST /api/v1/policies  { name, spec } -> 201 created policy.
  // The server validates the spec; a 400 surfaces as an HttpError.
  async createPolicy(name: string, spec: RunPolicySpec): Promise<RunPolicy> {
    const res = await wfetch("/policies", {
      method: "POST",
      body: JSON.stringify({ name, spec }),
    });
    return asJson<RunPolicy>(res);
  },

  // PUT /api/v1/policies/{id}  { name, spec } -> updated policy.
  async updatePolicy(id: string, name: string, spec: RunPolicySpec): Promise<RunPolicy> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ name, spec }),
    });
    return asJson<RunPolicy>(res);
  },

  // DELETE /api/v1/policies/{id} -> 204.
  async deletePolicy(id: string): Promise<void> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
