/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET and PUT /governance/approval-counts: how many approvals each kind of governance change needs.

// Who may approve a kind.
export type ApprovalCountsApprovers = "super_admin" | "security" | "security_or_unit_admin";

// Whether enough people can approve a kind now: ok, exact, short or cant_confirm.
export type ApprovalCountsPoolStatus = "ok" | "exact" | "short" | "cant_confirm";

export interface ApprovalCountsRule {
  required: number;
  require_security: boolean;
}

export interface ApprovalCountsPool {
  status: ApprovalCountsPoolStatus;
  count: number;
  // The unit a unit-scoped kind is counted in.
  unit_name?: string;
  // Only with cant_confirm.
  why?: "unconfirmed_people" | "too_many_people";
}

export interface ApprovalCountsKind extends ApprovalCountsRule {
  kind: string;
  // False when the kind follows the default.
  own: boolean;
  approvers: ApprovalCountsApprovers;
  pool: ApprovalCountsPool;
}

export interface ApprovalCounts {
  // The deploy-time minimum; 0 when it is not set.
  minimum: number;
  highest: number;
  default: ApprovalCountsRule;
  kinds: ApprovalCountsKind[];
}

export interface ApprovalCountsWriteKind extends ApprovalCountsRule {
  kind: string;
}

// The PUT body replaces every row: a kind it omits follows the default.
export interface ApprovalCountsWrite {
  default: ApprovalCountsRule;
  kinds: ApprovalCountsWriteKind[];
}
