/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approval requests — human-gated credential/egress/tool decisions.
export type ApprovalKind = "credential" | "egress_domain" | "tool_call";

export type ApprovalState = "PENDING" | "APPROVED" | "DENIED" | "EXPIRED";

// How far a human's approve/deny decision reaches — egress_domain carries all
// four; a credential carries only "run" (the per-run lease: a git_pat approved
// once is re-mintable for the rest of the run); a tool_call is clamped and
// carries none. Mirrors internal/types/types.go's ApprovalScope
// EXACTLY (Valid()'s four values) — hand-maintained, no parity test, so a
// value added on the Go side and not here is a silent client-side drift.
export type ApprovalScope = "once" | "run" | "until" | "always";

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
  // The two fields below are named `decision_*` ON THE WIRE deliberately —
  // NOT `scope`/`expires_at` — because `scope` would collide with
  // requested_scope above (the raised request's own JSON, part of the dedup
  // index) and a bare `expires_at` would collide with this table's existing
  // expiry concept (EXPIRED state, ExpireStale). See internal/types/types.go's
  // ApprovalRequest for the same naming note.
  //
  // Absent on a PENDING row (nothing decided yet) and on any row decided
  // before this feature shipped. Present only on an egress_domain decision.
  decision_scope?: ApprovalScope;
  // Set only when decision_scope is "until". ISO 8601; nothing server-side
  // transitions the row once this passes — the console derives "expired"
  // client-side (see copy.ts's approvalScopeBadge).
  decision_expires_at?: string;
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

// approve/deny's optional third argument — egress_domain only (the server
// 400s a scope on any other kind; see internal/api/approvals.go's rule 4).
// `until` is an ISO decision_expires_at, required by the server iff scope is
// "until".
export interface DecisionOptions {
  scope?: ApprovalScope;
  until?: string;
}

// Builds approve/deny's trailing args: NONE for the default "run" scope — a
// literal 2-argument call, byte-identical to the pre-scope wire body — and
// one DecisionOptions object otherwise. Every decision surface (live-
// approvals, run-detail's cockpit, the approvals console) MUST spread this
// rather than pass a possibly-undefined third positional arg: vitest's
// toHaveBeenCalledWith matches call arity exactly, so `approve(id, reason,
// undefined)` is a 3-element call and fails an assertion pinned to 2 — four
// such assertions exist across the suite.
//
// Deliberately in lib/types, NOT lib/api, even though it exists only to feed
// api/approvals.ts's approve/deny: every UI test that decides an approval
// mocks "lib/api/approvals" wholesale (a hand-written factory naming only
// `approvals.{listApprovals,approve,deny}`), so a second named export living
// in that module resolves to undefined under those mocks — the caller's
// `decisionArgs(...)` call throws, caught by the decide() try/catch, and the
// api call it was building never fires. lib/types is never mocked.
export function decisionArgs(scope: ApprovalScope, until?: string): [] | [DecisionOptions] {
  return scope === "run" ? [] : [{ scope, until }];
}
