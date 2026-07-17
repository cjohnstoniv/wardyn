/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval queue: list + approve/deny a pending credential/egress/tool request.
import type { ApprovalRequest } from "../types";
import { asJson, unwrapList, wfetch } from "./core";

export const approvals = {
  // GET /api/v1/approvals?state=<state>  (PENDING | APPROVED | DENIED | EXPIRED | "")
  async listApprovals(
    state: "PENDING" | "APPROVED" | "DENIED" | "EXPIRED" | "" = "PENDING",
  ): Promise<ApprovalRequest[]> {
    const qs = state ? `?state=${encodeURIComponent(state)}` : "";
    const res = await wfetch(`/approvals${qs}`, { method: "GET" });
    return unwrapList<ApprovalRequest>(await asJson<unknown>(res));
  },

  // POST /api/v1/approvals/{id}/approve  { reason }
  async approve(id: string, reason: string): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/approve`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    });
    return asJson<ApprovalRequest>(res);
  },

  // POST /api/v1/approvals/{id}/deny  { reason }
  async deny(id: string, reason: string): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/deny`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    });
    return asJson<ApprovalRequest>(res);
  },
};
