// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance profiles (migration 0052): the wire + store types for an
// ASSIGNABLE ceiling and the row that binds one to a subject.
//
// Named `governance*` throughout, deliberately: "profile" already means two
// unrelated things in this tree — the Recording-Mode synthesis result and the
// workspace scan profile — and a third would make every grep ambiguous at
// exactly the surface where a mistaken read is a containment bug.
package types

import (
	"time"

	"github.com/google/uuid"
)

// GovernanceLimits carries the autonomy switches that BOUND A REQUEST rather
// than a policy, which is why they live here and not in RunPolicySpec.
//
// The distinction is load-bearing. RunPolicySpec describes what a sandbox may
// reach once it is running, and every field of it is enforced OUTSIDE the
// sandbox by the proxy. The booleans instead refuse a run SHAPE before it
// exists, because each names a way to route AROUND the tool gate entirely, or
// (DenyUserDrive) a way for a run to leave state behind it:
//
//   - DenyTaskModeExec: task_mode=exec runs a bare command with no agent and no
//     toolgate in the loop, so no tool_rules ceiling can bind it. A profile that
//     wants supervised tool use has to be able to say "not through that door".
//   - DenyInteractive: an interactive run REFUSES tool_approvals=hold by design
//     (a human at the attach pane is the supervision), so a profile cannot
//     express "supervised" through tool_rules on that lane either. This is the
//     lever built for it.
//   - DenyUserDrive: a user drive is a tree that OUTLIVES the run, so a member
//     who may mount one can persist anything the sandbox produced past the
//     sandbox's own lifetime. No tool_rules ceiling describes that, because the
//     escape is the storage, not the tool.
//   - MaxConcurrentRuns is the odd one out — a QUOTA, not a door. It bounds how
//     many runs one member holds at once rather than what any single run may be,
//     which is why its enforcement site answers 422 with no authz.denied while
//     the booleans answer 403 with one.
//
// A CLOSED struct with `omitempty` on every field, not a map: the set is small,
// complete, and validated by the Go type itself, so migration 0052 puts no
// CHECK on the limits column at all (the 0042 doctrine — one closed Go
// definition, validated at the write boundary, zero DDL for the next member).
// EVERY zero value means "unrestricted", so a profile that omits limits behaves
// exactly as one written before this struct had fields — the absent-row
// back-compat rule applied one level down.
type GovernanceLimits struct {
	// DenyTaskModeExec refuses task_mode=exec for a member under this profile.
	DenyTaskModeExec bool `json:"deny_task_mode_exec,omitempty"`
	// DenyInteractive refuses an interactive run for a member under this
	// profile. Evaluated against POST-COERCION interactivity by its enforcement
	// site: a request that simply OMITS the task coerces to interactive later in
	// validation, so a raw "did the caller ask for interactive" read is evaded
	// by leaving a field out.
	DenyInteractive bool `json:"deny_interactive,omitempty"`
	// MaxConcurrentRuns caps how many NON-TERMINAL runs a member under this
	// profile may hold at once. 0 is unlimited — the same zero-value rule the
	// two booleans follow, so limits authored before this field existed keep
	// meaning what they meant.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// DenyUserDrive refuses a USER DRIVE mount for a member under this profile:
	// their run may not carry drive.enabled at all, whatever an admin has
	// allocated them.
	//
	// A DOOR, NOT A QUOTA, which is why it is a bool beside the other two
	// rather than a size beside MaxConcurrentRuns. A drive is a writable tree
	// that OUTLIVES the run — the one piece of state an agent can leave behind
	// — so "how big" is the wrong question for a ceiling to ask; "may this
	// principal persist anything at all" is the right one, and it is the same
	// shape as the two refusals above (403 with an authz.denied row, not a 422
	// quota answer).
	//
	// Zero means unrestricted, like every other field here: a profile written
	// before drives existed keeps meaning exactly what it meant, and a
	// deployment that never allocates a drive is unaffected either way.
	DenyUserDrive bool `json:"deny_user_drive,omitempty"`
}

// GovernanceProfile is one named, assignable ceiling (migration 0052's
// governance_profiles row).
//
// Ceiling is a full RunPolicySpec and is REPLACEMENT semantics, not composition:
// an assigned profile IS the principal's ceiling, and a principal with no
// assignment falls through to the deployment's Config.DefaultPolicy byte for
// byte. Composing the two would mean folding them through composer.Clamp, which
// is NOT a lattice meet (it drops workspace_mounts unconditionally, is
// order-dependent through llm_inspection, and defaults unnamed tools to hold) —
// so a "composed" ceiling would silently lose fields and could not express an
// autonomous profile at all.
//
// Name is the UNIQUE human handle: what an admin assigns by, what the console
// lists, and the last tie-break in the resolver's ORDER BY (so LIMIT 1 is
// deterministic even when priority ties).
type GovernanceProfile struct {
	ID        uuid.UUID        `json:"id"`
	Name      string           `json:"name"`
	Ceiling   RunPolicySpec    `json:"ceiling"`
	Limits    GovernanceLimits `json:"limits"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	CreatedBy string           `json:"created_by,omitempty"`
}

// GovernanceAssignment binds one profile to one subject (migration 0052's
// governance_assignments row).
//
// SubjectType REUSES CapabilitySubjectType — the same user/group/all vocabulary
// capability_grants is written against, and the same one capabilitySubjects
// resolves a caller into. A second enum meaning the same three things is the
// dual-matcher drift this codebase already warns about elsewhere: two
// definitions of "who" that disagree by one case is how a deny stops biting.
//
// Priority breaks ties WITHIN a tier (higher wins) — the group tier is where it
// earns its keep, since a member is typically in several groups at once and the
// admin needs to say which group's profile is the operative one. It does NOT
// cross tiers: a user-tier row beats every group-tier row at any priority,
// because an assignment is one admin explicitly naming one principal.
type GovernanceAssignment struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	ProfileID   uuid.UUID             `json:"profile_id"`
	Priority    int                   `json:"priority"`
	CreatedAt   time.Time             `json:"created_at"`
	CreatedBy   string                `json:"created_by,omitempty"`
}
