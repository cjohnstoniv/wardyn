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
  // GET /api/v1/approvals?state=<state>&run_id=<id>
  //   state: PENDING | APPROVED | DENIED | EXPIRED | "" (every state)
  //   runId: "" (every run) or one run's id.
  //
  // runId is NOT a convenience. internal/api/approvals.go:56-61 spells out why
  // the predicate has to run server-side: the list is requested_at DESC and
  // capped at maxListLimit, and ?run_id= filters INSIDE the fetch-all closure,
  // i.e. BEFORE that window is applied. A caller that instead pulls the
  // un-scoped list and filters in the browser filters AFTER the window, so past
  // maxListLimit lifetime approvals an older run's own rows vanish from its
  // detail page — silently, with a PENDING badge of 0. Any per-run consumer
  // (run detail, the live strip) MUST pass runId rather than post-filter.
  async listApprovals(
    state: "PENDING" | "APPROVED" | "DENIED" | "EXPIRED" | "" = "PENDING",
    runId = "",
  ): Promise<ApprovalRequest[]> {
    const params = new URLSearchParams();
    if (state) params.set("state", state);
    if (runId) params.set("run_id", runId);
    const qs = params.toString();
    const res = await wfetch(withLimit(`/approvals${qs ? `?${qs}` : ""}`), { method: "GET" });
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
