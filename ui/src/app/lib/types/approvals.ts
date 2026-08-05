/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval requests — human-gated credential/egress/tool decisions.
export type ApprovalKind = "credential" | "egress_domain" | "tool_call";

export type ApprovalState = "PENDING" | "APPROVED" | "DENIED" | "EXPIRED";

export interface ApprovalRequest {
  id: string;
  run_id: string;
  grant_id?: string;
  kind: ApprovalKind;
  // Real wire field is free-form JSON; keep `unknown` and let the screens
  // narrow it. Index signature lets UI read arbitrary keys safely.
  requested_scope: Record<string, unknown>;
  state: ApprovalState;
  requested_at: string;
  decided_at?: string;
  decided_by?: string;
  minted_jti?: string;
  reason?: string;
}

// canDecideApproval mirrors internal/api/approvals.go's decide() exactly: an
// operator/admin may decide anything; a member may decide only an
// egress_domain approval — credential and tool_call stay admin-only
// REGARDLESS of ownership (self-approving your own run's credential mint or
// re-opening the clamp's tool_call bound would be self-authorizing under the
// operator's own ceiling). Both callers (approvals.tsx, run-detail.tsx) already
// only ever render rows the caller owns (the list itself is server-scoped —
// see handleListApprovals's creator-pager branch / a run-detail page's
// getRunAuthorized gate), so ownership is a precondition of the row existing
// at all, not something this predicate needs to re-check.
export function canDecideApproval(operator: boolean, kind: ApprovalKind): boolean {
  return operator || kind === "egress_domain";
}
