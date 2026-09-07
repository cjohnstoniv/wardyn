/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval queue: list + approve/deny a pending credential/egress/tool request.
// DecisionOptions/decisionArgs live in lib/types/approvals.ts, not here — see
// that file's comment on decisionArgs for why (every UI test that decides an
// approval mocks THIS module wholesale, which would silently break a second
// named export added here). The consequence of that rule is that NO consumer
// test ever observes the body approve()/deny() build, and the daemon decodes
// the decision body BY HAND without DisallowUnknownFields (internal/api/
// approvals.go's decodeDecisionRequest), so a wrong key is dropped in silence
// and Normalize() keeps the default `run` scope — no 400, no log, nothing
// visible. approvals.wire.test.ts is therefore the ONLY thing pinning the two
// decision_* key names below; it exercises this module for real (fetch stubbed,
// module unmocked) and reads the Go json tags out of internal/api/approvals.go.
import type { ApprovalRequest, DecisionOptions } from "../types";
import { asJson, unwrapList, wfetch, withLimit } from "./core";

export const approvals = {
  // GET /api/v1/approvals?state=<state>  (PENDING | APPROVED | DENIED | EXPIRED | "")
  async listApprovals(
    state: "PENDING" | "APPROVED" | "DENIED" | "EXPIRED" | "" = "PENDING",
  ): Promise<ApprovalRequest[]> {
    const qs = state ? `?state=${encodeURIComponent(state)}` : "";
    const res = await wfetch(withLimit(`/approvals${qs}`), { method: "GET" });
    return unwrapList<ApprovalRequest>(await asJson<unknown>(res));
  },

  // POST /api/v1/approvals/{id}/approve  { reason, decision_scope?, decision_expires_at? }
  async approve(id: string, reason: string, opts?: DecisionOptions): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/approve`, {
      method: "POST",
      body: JSON.stringify({
        reason,
        ...(opts?.scope ? { decision_scope: opts.scope } : {}),
        ...(opts?.until ? { decision_expires_at: opts.until } : {}),
      }),
    });
    return asJson<ApprovalRequest>(res);
  },

  // POST /api/v1/approvals/{id}/deny  { reason, decision_scope?, decision_expires_at? }
  async deny(id: string, reason: string, opts?: DecisionOptions): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/deny`, {
      method: "POST",
      body: JSON.stringify({
        reason,
        ...(opts?.scope ? { decision_scope: opts.scope } : {}),
        ...(opts?.until ? { decision_expires_at: opts.until } : {}),
      }),
    });
    return asJson<ApprovalRequest>(res);
  },
};
