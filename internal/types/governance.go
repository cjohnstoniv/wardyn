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
	"fmt"
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
//   - MaxEphemeralDiskMiB and MaxDriveSizeMiB are the two SIZES (0.7.2), and
//     they are neither doors nor quotas: they CLAMP. A run or a drive at the
//     bound is capped and told so, never refused — `disk_mib` is authored on
//     policies, so a 422 would break every stored policy the day a limit is
//     first written. Their enforcement sites are the two places that hold both
//     the profile and the deployment ceiling: dispatch for the scratch size,
//     newResolvedDrive for the drive.
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
	// MaxEphemeralDiskMiB caps the EPHEMERAL scratch a member's run under this
	// profile may be given — the writable layer a sandbox gets when it mounts no
	// drive. 0 is unlimited, the same zero-value rule every field here follows.
	//
	// A CLAMP, NOT A DOOR, which is why it is a size beside MaxConcurrentRuns
	// rather than a bool beside the three refusals. `disk_mib` is authored on
	// POLICIES, so refusing a run that asks for more would break every stored
	// policy the day an admin first writes a limit; the run is capped and told
	// so, in composer.Clamp's own idiom ("resources capped to operator
	// maximum"). An authorized caller at a bound therefore earns no
	// authz.denied row — nothing was denied.
	//
	// ENFORCED AT DISPATCH, in ONE place (runs_dispatch.go, beside the ceiling
	// deny re-assertion), folded together with the deployment's own
	// storage.ephemeral.max_disk_mib ceiling — never on the create path, whose
	// resourceLimitsToRunner is a pure mapper with neither the ceiling nor the
	// site config in scope. Assigned members only; operators are exempt.
	//
	// WHETHER THE CAP BINDS depends on the substrate: it is a request the runner
	// makes of Kubernetes or Docker, and Docker's overlay2 does not enforce a
	// size at all. Render it through StorageEnforcement and never claim a cap
	// the substrate does not keep.
	MaxEphemeralDiskMiB int `json:"max_ephemeral_disk_mib,omitempty"`
	// MaxDriveSizeMiB caps how large a USER DRIVE may be for a member under this
	// profile. 0 is unlimited. The governance twin of DenyUserDrive: that field
	// answers "may this principal persist anything at all", this one answers
	// "how much" — and a profile can carry either without the other.
	//
	// PER PRINCIPAL, and that is an open question the shape argument has to
	// name: a drive today is one tree belonging to one subject, so a ceiling on
	// its size is a ceiling on that person. Team-shared drives (0.8) add a scope
	// axis on the same row rather than a second field — a shared drive's ceiling
	// is not the sum of its members' and must not be derived from one. Until
	// then, "per principal" is the whole meaning.
	//
	// CLAMPED IN newResolvedDrive (user_drives_resolve.go), the one scope that
	// holds BOTH facts — never at grant write, where the profile binding a
	// subject is claims-resolved and unreadable from the row. It folds with the
	// deployment's own storage.user_drive.max_size_mib in one min() expression,
	// so launch, /me and POST /drives/preview cannot disagree about a drive's
	// size.
	MaxDriveSizeMiB int `json:"max_drive_size_mib,omitempty"`
	// AutonomyRubric maps a run's posture to a permitted AutonomyLevel for a
	// member under this profile (0.8, #77). A POINTER: `omitempty` never omits a
	// struct value, so a plain (non-pointer) field would put
	// `"autonomy_rubric":{}` on every profile's wire body, including one
	// authored before this field existed — TestGovernanceLimitsWireRoundTrip
	// pins that an unrestricted profile still marshals `limits: {}` byte for
	// byte, the same zero-value rule every field above follows. Nil means "no
	// rubric": resolveRunAutonomy (#97) treats it exactly like a member with no
	// assigned profile at all.
	AutonomyRubric *AutonomyRubric `json:"autonomy_rubric,omitempty"`
}

// AutonomyLevel is one rung on the autonomy ladder a governance profile's
// AutonomyRubric caps against, L0 (most supervised) through L3 (least). The
// codes stay internal — the console renders plain labels — the same way
// ConfinementClass's CC1/CC2/CC3 do (0.8 #77):
//
//   - AutonomyL0 "attended": interactive only, supervised seeding.
//   - AutonomyL1 "gated": adds non-interactive runs, but tool approvals are
//     derived to `hold`.
//   - AutonomyL2 "unattended": adds auto-approval and seeded auto tools.
//   - AutonomyL3: adds `task_mode=exec`, the door that routes around every
//     other gate, so it is the top rung.
type AutonomyLevel string

const (
	AutonomyL0 AutonomyLevel = "L0"
	AutonomyL1 AutonomyLevel = "L1"
	AutonomyL2 AutonomyLevel = "L2"
	AutonomyL3 AutonomyLevel = "L3"
)

// Valid reports whether l is one of the four defined rungs. Unlike
// ConfinementClass (which has no such gate — every caller of Rank already
// tolerates rank 0), AutonomyRubric needs one: an author-facing field, so a
// typo must 400 rather than silently rank as "below L0".
func (l AutonomyLevel) Valid() bool {
	switch l {
	case AutonomyL0, AutonomyL1, AutonomyL2, AutonomyL3:
		return true
	}
	return false
}

// Rank orders AutonomyLevel weakest (most supervised) -> strongest (least),
// mirroring ConfinementClass.Rank(): resolving a rubric folds several
// applicable caps to their MINIMUM level (#97), and Rank is what "minimum"
// compares on. An unrecognised value ranks below L0 so it never wins a min()
// against a real level.
func (l AutonomyLevel) Rank() int {
	switch l {
	case AutonomyL0:
		return 0
	case AutonomyL1:
		return 1
	case AutonomyL2:
		return 2
	case AutonomyL3:
		return 3
	default:
		return -1
	}
}

// AutonomyRubric maps a run's posture to a permitted AutonomyLevel. Nine
// closed fields — three egress postures, three secret postures, three
// confinement classes — each unset (that posture caps nothing) or one of the
// four levels (0.8 #77's design: "nine closed fields ... each unset or a
// level"). The level a run resolves to is the MINIMUM over every field whose
// posture applies (internal/composer/autonomy.go, #97); an all-unset rubric
// caps nothing, identically to a nil rubric.
//
// A closed struct with `omitempty` on every field, not a map — the same
// GovernanceLimits doctrine this type lives inside of: the set is small,
// complete, and validated by the Go type itself (Validate), so no DDL CHECK
// backs the stored JSON column at all.
type AutonomyRubric struct {
	// EgressOpen caps the level when the run's egress is OPEN: allow-all, or
	// any allowlisted host beyond baseline.
	EgressOpen AutonomyLevel `json:"egress_open,omitempty"`
	// EgressReviewed caps the level when egress is REVIEWED: first-use approval
	// raises approvals, but nothing is wide open.
	EgressReviewed AutonomyLevel `json:"egress_reviewed,omitempty"`
	// EgressSealed caps the level when egress is SEALED: neither of the above.
	EgressSealed AutonomyLevel `json:"egress_sealed,omitempty"`
	// SecretsPowerful caps the level when the run holds a POWERFUL secret: any
	// write-capable grant, an api_key to a non-baseline host, or a
	// git_pat/ssh_key/env_secret grant.
	SecretsPowerful AutonomyLevel `json:"secrets_powerful,omitempty"`
	// SecretsBaseline caps the level when the run holds any grant, none of them
	// powerful.
	SecretsBaseline AutonomyLevel `json:"secrets_baseline,omitempty"`
	// SecretsNone caps the level when the run holds no grant at all.
	SecretsNone AutonomyLevel `json:"secrets_none,omitempty"`
	// ConfinementCC1/CC2/CC3 cap the level by the run's ENFORCED confinement
	// class (an empty enforced class reads as CC1 — see AutonomyPosture).
	ConfinementCC1 AutonomyLevel `json:"confinement_cc1,omitempty"`
	ConfinementCC2 AutonomyLevel `json:"confinement_cc2,omitempty"`
	ConfinementCC3 AutonomyLevel `json:"confinement_cc3,omitempty"`
}

// Validate reports the first field carrying a value that is not a defined
// AutonomyLevel, NAMING that field — governanceLimitsRefusal
// (internal/api/governance.go) prefixes the field name onto its "limits."
// 400 so an admin is told which of the nine to fix, not just "invalid". An
// empty field is always valid: unset means "this posture caps nothing".
func (a AutonomyRubric) Validate() error {
	for _, f := range []struct {
		field string
		level AutonomyLevel
	}{
		{"egress_open", a.EgressOpen},
		{"egress_reviewed", a.EgressReviewed},
		{"egress_sealed", a.EgressSealed},
		{"secrets_powerful", a.SecretsPowerful},
		{"secrets_baseline", a.SecretsBaseline},
		{"secrets_none", a.SecretsNone},
		{"confinement_cc1", a.ConfinementCC1},
		{"confinement_cc2", a.ConfinementCC2},
		{"confinement_cc3", a.ConfinementCC3},
	} {
		if f.level != "" && !f.level.Valid() {
			return fmt.Errorf("%s: %q is not a valid autonomy level", f.field, f.level)
		}
	}
	return nil
}

// AutonomyEgressPosture is one of the three egress states an AutonomyRubric
// caps against (internal/composer/autonomy.go's posture arithmetic, #97).
type AutonomyEgressPosture string

const (
	AutonomyEgressOpen     AutonomyEgressPosture = "open"
	AutonomyEgressReviewed AutonomyEgressPosture = "reviewed"
	AutonomyEgressSealed   AutonomyEgressPosture = "sealed"
)

// AutonomySecretsPosture is one of the three secret-power states an
// AutonomyRubric caps against.
type AutonomySecretsPosture string

const (
	AutonomySecretsPowerful AutonomySecretsPosture = "powerful"
	AutonomySecretsBaseline AutonomySecretsPosture = "baseline"
	AutonomySecretsNone     AutonomySecretsPosture = "none"
)

// AutonomyPosture is a run's three-axis shape — egress reach, secret power,
// enforced confinement class — the input #97's resolveRunAutonomy folds
// against a profile's AutonomyRubric to pick an AutonomyLevel. Computed, never
// stored on its own; it travels inside AutonomyResolution.
type AutonomyPosture struct {
	Egress      AutonomyEgressPosture  `json:"egress"`
	Secrets     AutonomySecretsPosture `json:"secrets"`
	Confinement ConfinementClass       `json:"confinement"`
}

// AutonomyResolution is what resolveRunAutonomy (#97) decides for one run: the
// level, the posture that produced it, and which rubric field bound the
// result — the "level, the posture and what bound it" #77 asks to be
// provenance on the create audit row, the frozen AgentRun.AutonomyLevel, and
// the preflight response. Bound is empty when nothing capped the level (no
// profile, no rubric, or a posture the rubric left unset) — the zero value
// throughout, matching every other GovernanceLimits field's "empty means
// unrestricted" rule.
type AutonomyResolution struct {
	Level   AutonomyLevel   `json:"level"`
	Posture AutonomyPosture `json:"posture"`
	Bound   string          `json:"bound,omitempty"`
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
