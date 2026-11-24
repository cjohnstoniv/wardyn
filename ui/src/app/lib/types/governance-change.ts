/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { BoundaryReport } from "../api/boundary";
import type { TreeDryRunPerson } from "../api/governance";
import type { RiskItem, RiskLevel } from "./runs";

// types.GovernanceChange (0.8.6, internal/types/governance_change.go) — one governance write held for a
// second human. The body of a covered write's `202 {pending_change}` and the row of
// GET /governance/changes. The payload, hashes and emails never reach a response.
export type GovernanceChangeState = "pending" | "applied" | "rejected" | "expired" | "stale";

export interface GovernanceChange {
  id: string;
  // governance_unit, governance_membership, capability_grant, capability_enforcement,
  // capability_availability, user_type_priority, role_mapping, key_domain_assignment, egress_baseline,
  // runner_pool_use_policy, service_identity, governance_unit_admin (and later kinds): kept a string so a kind this console
  // predates still lists, under its raw name.
  target_kind: string;
  // create, update, delete, upsert, replace or set; a membership change may also be move or promote.
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
  // The proposer's reason and ticket (PUT /governance/changes/{id}/note); absent when none was written.
  // Free text: render it as text, never a link.
  note?: GovernanceChangeNote;
  // GET /governance/changes/{id}: who could decide it, the proposer left out (changeApprover,
  // internal/api/governance_unit_admin.go).
  approvers?: GovernanceChangeApprover[];
  // POST /governance/changes/{id}/approve only: the change is applied, but it could not revoke every API token
  // that may still hold the role it took away; detail is the server's remedy.
  tokens_revocation_failed?: boolean;
  // POST /governance/changes/{id}/approve only: a service identity suspension is applied, but it could not end
  // every live run of the identity; detail is the server's remedy.
  runs_end_failed?: boolean;
  detail?: string;
  // A 202's pending_change only: advisory sentences for the proposer (a deciders rule under which a host rule
  // hold is decided once at most). Not stored with the change.
  warnings?: string[];
}

// types.ChangeNote (internal/types/governance_change_note.go).
export interface GovernanceChangeNote {
  reason?: string;
  ticket?: string;
}

// One who could decide a held change: any security admin, a unit-admin grant (a person or a group)
// on unit_name that governs every unit the change touches, or, for a request, anyone in the group name names.
export interface GovernanceChangeApprover {
  kind: "security" | "unit_admin" | "group";
  name?: string;
  unit_name?: string;
  subject_type?: "user" | "group";
}

// governanceDiffBody (internal/api/governance_change_gate.go). before is absent on a create and after on
// a delete. An assignment change's `after.profile` is the profile it assigns, as it stood at proposal.
export interface GovernanceChangeDiff {
  before?: unknown;
  after?: unknown;
  changed: string[];
  // Advisory sentences the server wrote about the proposed change (a profile's omission warnings); absent when none.
  notes?: string[];
  // A legacy membership delete or move: the units whose legacy memberships may become someone's context.
  activates?: { unit_id: string; unit_name: string }[];
  // A legacy membership move: the units it may take someone out of.
  vacates?: { unit_id: string; unit_name: string }[];
  // What the server read of the change at proposal: whether it widens, and the risk grade. Absent on a request
  // kind and on a change proposed before the server wrote it; the console reads an absent review on any other
  // kind as not_checked.
  review?: ChangeReview;
}

// The server's reading of a proposed change.
export type ChangeVerdict = "widens" | "narrows_held" | "not_checked";

// Why a change that does not widen is still held for a person.
export type HeldBecause = "contact" | "lock_removed" | "rename" | "legacy_shift" | "first_allowlist";

// One field the change widens, with the entries it adds. field is a policy field's JSON name, "limits",
// "member_cap" (entries are the raised keys), "members", "withheld_grants", "vcs" or "owning_unit".
export interface ChangeWidening {
  field: string;
  entries?: string[];
}

// The graded risk of one side of a profile change, deterministic and never a model's.
export interface ChangeRiskSide {
  overall: RiskLevel;
  items: RiskItem[];
}

export interface ChangeReview {
  verdict: ChangeVerdict;
  widens?: ChangeWidening[];
  held_because?: HeldBecause[];
  // Only for a profile create (before is null), a profile update and a unit admin's membership move.
  risk?: { before: ChangeRiskSide | null; after: ChangeRiskSide };
}

// One live run a change would reach. title is sent only to the run's owner and to admins.
export interface ChangeImpactRun {
  id: string;
  owner: string;
  started_at: string;
  title?: string;
}

// GET /governance/changes/{id}/impact: who and what an approval would reach, as of as_of.
export interface ChangeImpact {
  computed: "tree" | "boundary" | "none";
  // Only with computed "none": the kind has no replay, the change went stale, or the viewer may not see it.
  not_computed?: "kind" | "stale" | "refused";
  as_of: string;
  // computed "tree": always an array.
  people?: TreeDryRunPerson[];
  live_runs?: ChangeImpactRun[];
  // Every live run the change reaches; live_runs lists at most 50.
  live_runs_total?: number;
  activates?: { unit_id: string; unit_name: string }[];
  vacates?: { unit_id: string; unit_name: string }[];
  // computed "boundary" only.
  boundary?: BoundaryReport;
}
