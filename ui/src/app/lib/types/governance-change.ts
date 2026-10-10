/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// types.GovernanceChange (0.8.6, internal/types/governance_change.go) — one governance write held for a
// second human. The body of a covered write's `202 {pending_change}` and the row of
// GET /governance/changes. The payload, hashes and emails never reach a response.
export type GovernanceChangeState = "pending" | "applied" | "rejected" | "expired" | "stale";

export interface GovernanceChange {
  id: string;
  // governance_profile, governance_assignment, capability_grant, capability_enforcement,
  // capability_availability, user_type_priority, role_mapping, key_domain_assignment, egress_baseline,
  // runner_pool_use_policy (and later kinds): kept a string so a kind this console predates still lists, under its raw name.
  target_kind: string;
  // create, update, delete, upsert, replace or set.
  op: string;
  target_key: string;
  state: GovernanceChangeState;
  proposed_by: string;
  proposed_at: string;
  expires_at: string;
  // The server-rendered diff: the redacted before and after views and the changed field paths. The
  // console reads it and never computes a diff of its own.
  diff: GovernanceChangeDiff;
  decided_by?: string;
  decided_at?: string;
  reason?: string;
}

// governanceDiffBody (internal/api/governance_change_gate.go). before is absent on a create and after on
// a delete. An assignment change's `after.profile` is the profile it assigns, as it stood at proposal.
export interface GovernanceChangeDiff {
  before?: unknown;
  after?: unknown;
  changed: string[];
}
