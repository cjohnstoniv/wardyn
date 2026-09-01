/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run policies (admin-gated config): list/create/update/delete.
import type { RunPolicy, RunPolicySpec } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch, withLimit } from "./core";

// GET /policies/default's body (internal/api/policies.go's
// defaultPolicyResponse): the resolved ceiling spec, EMBEDDED so the shape
// every existing consumer parses is unchanged, plus the name of the governance
// profile it came from.
//
// governance_profile_name is ADDITIVE and OMITTED ENTIRELY for a caller with no
// assignment — absent, never "". That is what lets the member surfaces follow
// the absent-row doctrine: no profile ⇒ no chip, no line, no placeholder, and
// today's screens byte for byte.
export type DefaultPolicy = RunPolicySpec & { governance_profile_name?: string };

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

  // GET /api/v1/policies/default — THE CALLER'S ceiling: the spec a run created
  // without a policy_id gets, and the ceiling their inline policy is clamped
  // against. W14-S1-6; routed through effectiveCeiling since 0.7, so for a
  // member under a governance profile this answers with THAT profile's spec.
  async getDefaultPolicy(): Promise<DefaultPolicy> {
    const res = await wfetch("/policies/default", { method: "GET" });
    return asJson<DefaultPolicy>(res);
  },

  // DELETE /api/v1/policies/{id} -> 204.
  async deletePolicy(id: string): Promise<void> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
