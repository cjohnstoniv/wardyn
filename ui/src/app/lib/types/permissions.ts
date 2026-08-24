/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Permissioning wire types (0.6 pillar 2) — mirrors internal/types.CapabilityGrant
// and the two response bodies in internal/api/permissions.go. `capability` is a
// plain string on the wire for the same reason it is in Go: the closed kind set
// lives in ONE place per side (capabilityKinds there, CAPABILITY_KINDS in
// lib/permissions-copy.ts here), and a stored kind this build doesn't know is
// inert rather than a parse error.

export type CapabilitySubjectType = "user" | "group" | "all";
export type CapabilityEffect = "allow" | "deny";

// One row of the grant table: "subject S may (or may not) use capability C at
// value V". `subject` is "" for subject_type "all" (there is nothing for it to
// name); user/group subjects are stored lowercased server-side.
export interface CapabilityGrant {
  id: string;
  subject_type: CapabilitySubjectType;
  subject: string;
  capability: string;
  value: string;
  effect: CapabilityEffect;
  created_at: string;
  created_by?: string;
}

// GET /permissions — the admin screen's whole data need in one call.
// `enforcement` is the per-kind switch map; an ABSENT key means "not enforced",
// which is the zero-config default an upgraded 0.5 deployment carries.
export interface PermissionsSnapshot {
  grants: CapabilityGrant[];
  enforcement: Record<string, boolean>;
}

// GET /me/capabilities — the member-safe twin: only what the CALLER holds.
// groups_snapshot_stale is the nil-vs-empty distinction: a session recorded
// before Wardyn stamped groups can't have its group grants resolved at all,
// which reads as "can't tell yet", never as "holds no group grants".
export interface MeCapabilities {
  grants: CapabilityGrant[];
  enforcement: Record<string, boolean>;
  session_groups: string[];
  groups_snapshot_stale: boolean;
}

// POST /permissions/grants — the natural key plus the effect. Identity
// (id/created_at/created_by) is always server-assigned and never sent.
export interface CapabilityGrantInput {
  subject_type: CapabilitySubjectType;
  subject: string;
  capability: string;
  value: string;
  effect: CapabilityEffect;
}
