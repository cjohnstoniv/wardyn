/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The held-request routes (internal/api/governance_requests.go): what the caller may ask for, the requests
// they may see, and a decider's answer. Types: lib/types/governance-requests.ts.

import type { GovernanceChange } from "../types/governance-change";
import type { ElevationOffer, GovernanceRequest, GovernanceRequestBody, GovernanceRequestList } from "../types/governance-requests";
import { asJson, unwrapList, wfetch } from "./core";

export const requests = {
  // GET /api/v1/governance/requests -> the requests the caller may see and what they may ask for. A 404 is an
  // older server that has no such route: null, so a screen offers nothing instead of failing.
  async list(): Promise<GovernanceRequestList | null> {
    const res = await wfetch("/governance/requests", { method: "GET" });
    if (res.status === 404) return null;
    const body = await asJson<GovernanceRequestList>(res);
    return {
      requests: unwrapList<GovernanceRequest>(body.requests),
      offered: unwrapList<ElevationOffer>(body.offered),
      image_offered: body.image_offered === true,
    };
  },

  // POST /api/v1/governance/requests -> 202 {pending_change, request}. This route always holds the request for
  // a decider, so its success is a 202 with `pending_change`, which asJson would throw as a PendingChangeError.
  // The success body is read here instead; a refusal still goes through asJson, so it is the same HttpError.
  async create(body: GovernanceRequestBody): Promise<GovernanceRequest> {
    const res = await wfetch("/governance/requests", { method: "POST", asPerson: true, body: JSON.stringify(body) });
    if (!res.ok) await asJson<never>(res);
    return ((await res.json()) as { request: GovernanceRequest }).request;
  },

  // POST /api/v1/governance/requests/{id}/approve -> 200 the change, applied.
  async approve(id: string): Promise<GovernanceChange> {
    const res = await wfetch(`/governance/requests/${encodeURIComponent(id)}/approve`, { method: "POST", asPerson: true });
    return asJson<GovernanceChange>(res);
  },

  // POST /api/v1/governance/requests/{id}/reject -> 200 the change, rejected. The reason is optional. The
  // proposer's own reject is a withdrawal.
  async reject(id: string, reason: string): Promise<GovernanceChange> {
    const res = await wfetch(`/governance/requests/${encodeURIComponent(id)}/reject`, {
      method: "POST",
      asPerson: true,
      body: JSON.stringify(reason ? { reason } : {}),
    });
    return asJson<GovernanceChange>(res);
  },
};
