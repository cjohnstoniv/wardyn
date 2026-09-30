// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance profiles (migration 0052): the wire + store types for an
// ASSIGNABLE ceiling and the row that binds one to a subject.
//
// Named `governance*` throughout since "profile" already means two other
// things in this tree (Recording-Mode synthesis, workspace scan profile),
// and grep ambiguity here is a containment-bug risk.
package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// GovernanceLimits carries the autonomy switches that BOUND A REQUEST rather
// than a policy (RunPolicySpec bounds what a sandbox may reach once
// running, enforced by the proxy). These refuse a run SHAPE before it
// exists, or (DenyUserDrive) leave state behind:
//
//   - DenyTaskModeExec: task_mode=exec has no agent and no toolgate, so no
//     tool_rules ceiling binds it.
//   - DenyInteractive: an interactive run refuses tool_approvals=hold by
//     design, so tool_rules can't express "supervised" there either.
//   - DenyUIApps: a UI app is relay access into the sandbox that no
//     tool_rules ceiling sees.
//   - DenyUserDrive: a drive OUTLIVES the run, so no tool_rules ceiling
//     describes the escape — the storage itself is the escape.
//   - MaxConcurrentRuns is a QUOTA, not a door: 422 with no authz.denied,
//     not 403.
//   - MaxEphemeralDiskMiB/MaxDriveSizeMiB are neither: they CLAMP — capped
//     and told so, never refused (disk_mib is policy-authored, so refusing
//     would break every stored policy the day a limit is first written).
//
// A CLOSED struct with `omitempty` on every field, not a map: small,
// complete, validated by the Go type itself, no DB CHECK needed. Every zero
// value means "unrestricted", so an omitted limits object behaves exactly
// as before this struct existed.
type GovernanceLimits struct {
	// DenyTaskModeExec refuses task_mode=exec for a member under this profile.
	DenyTaskModeExec bool `json:"deny_task_mode_exec,omitempty"`
	// DenyInteractive refuses an interactive run under this profile.
	// Evaluated against POST-COERCION interactivity, since omitting the
	// task coerces to interactive later — a raw "did the caller ask" check
	// would be evaded by leaving the field out. It also refuses the terminal
	// attach and SSH into a run created under this profile, since a task or
	// exec run still has a sandbox to open a shell in (#1392).
	DenyInteractive bool `json:"deny_interactive,omitempty"`
	// DenyUIApps strips ui_apps from a run under this profile at create, and
	// the UI gateway refuses a session into a run created under it. A limit
	// rather than an empty ceiling ui_apps, which stays "no opinion" so
	// existing profiles keep their behaviour (#1391).
	DenyUIApps bool `json:"deny_ui_apps,omitempty"`
	// MaxConcurrentRuns caps how many NON-TERMINAL runs a member under this
	// profile may hold at once. 0 is unlimited.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// DenyUserDrive refuses a USER DRIVE mount under this profile, whatever
	// an admin allocated. A DOOR, NOT A QUOTA: a drive outlives the run, so
	// "may this principal persist anything at all" is the question (403 +
	// authz.denied), not "how big".
	DenyUserDrive bool `json:"deny_user_drive,omitempty"`
	// MaxEphemeralDiskMiB caps the EPHEMERAL scratch (the writable layer when no drive is
	// mounted) a run under this profile may be given. 0 is unlimited. A CLAMP, NOT A DOOR: no
	// authz.denied at the bound. Enforced at dispatch only (runs_dispatch.go), folded with the
	// deployment's own storage.ephemeral.max_disk_mib; assigned members only, operators exempt.
	// Whether it actually binds depends on the substrate (Docker's overlay2 doesn't enforce
	// size at all) — render through StorageEnforcement, never claim a cap the substrate doesn't keep.
	MaxEphemeralDiskMiB int `json:"max_ephemeral_disk_mib,omitempty"`
	// MaxDriveSizeMiB caps how large a USER DRIVE may be under this profile. 0 is unlimited —
	// DenyUserDrive's twin ("may persist" vs "how much"). PER PRINCIPAL: a drive is one tree
	// belonging to one subject; a team-shared drive's ceiling is not the sum of its members'
	// and can't be derived from one, so sharing (0.8) adds a scope axis instead. Clamped in
	// newResolvedDrive, the one place holding both facts — never at grant write, where the
	// subject is claims-resolved and unreadable from the row. Folds with
	// storage.user_drive.max_size_mib in one min(), so launch, /me and preview can't disagree.
	MaxDriveSizeMiB int `json:"max_drive_size_mib,omitempty"`
	// AutonomyRubric maps a run's posture to a permitted AutonomyLevel
	// (0.8, #77). A POINTER so an unset rubric doesn't put
	// `"autonomy_rubric":{}` on every profile's wire body. Nil means "no
	// rubric" (resolveRunAutonomy, #97, treats it like no assigned profile).
	AutonomyRubric *AutonomyRubric `json:"autonomy_rubric,omitempty"`
	// RunLimits is embedded, so its seven fields sit flat on the limits wire
	// object beside the ones above — the same field set a run captures at
	// create (AgentRun.RunLimits).
	RunLimits
}

// RunLimits bound how long a run lives and how long it waits for a decision
// (long-holds design rev 4, §2.2). Zero values keep today's behaviour: no
// end, the deployment's approval expiry as the wait, no idle pause.
//
// They bind every run under a profile, including a security admin's; only a
// super admin's run skips them (effectiveCeiling resolves no profile for an
// operator).
type RunLimits struct {
	// MaxEndAheadSec: furthest ahead of NOW a run's end may be set (0 = no limit).
	// Extending within it never needs UserChangesLimits — extending is the lease.
	MaxEndAheadSec int `json:"max_end_ahead_sec,omitempty"`
	// DefaultEndSec is a new run's end, from create; 0 means MaxEndAheadSec, and
	// a run has no end when both are 0.
	DefaultEndSec int `json:"default_end_sec,omitempty"`
	// AllowNoEnd offers "No end" to a user who may change limits.
	AllowNoEnd bool `json:"allow_no_end,omitempty"`
	// MaxWaitSec/DefaultWaitSec: longest and default wait for a decision; 0 =
	// the deployment's approval expiry, which also caps both.
	MaxWaitSec     int `json:"max_wait_sec,omitempty"`
	DefaultWaitSec int `json:"default_wait_sec,omitempty"`
	// UserChangesLimits is the one gate: the user may shorten the end, set No
	// end, or change the wait.
	UserChangesLimits bool `json:"user_changes_limits,omitempty"`
	// PauseIdleAfterSec pauses an unused run after this long (0 pauses only
	// runs waiting for a decision).
	PauseIdleAfterSec int `json:"pause_idle_after_sec,omitempty"`
}

// AutonomyLevel is one rung on the autonomy ladder a governance profile's
// AutonomyRubric caps against, L0 (most supervised) through L3 (least). The
// codes stay internal — the console renders plain labels, like
// ConfinementClass's CC1/CC2/CC3 (0.8 #77):
//
//   - L0 "attended": interactive only, supervised seeding.
//   - L1 "gated": adds non-interactive runs, but tool approvals derive to hold.
//   - L2 "unattended": adds auto-approval and seeded auto tools.
//   - L3: adds task_mode=exec, the door around every other gate — the top rung.
type AutonomyLevel string

const (
	AutonomyL0 AutonomyLevel = "L0"
	AutonomyL1 AutonomyLevel = "L1"
	AutonomyL2 AutonomyLevel = "L2"
	AutonomyL3 AutonomyLevel = "L3"
)

// Valid reports whether l is one of the four defined rungs. Unlike
// ConfinementClass (whose every Rank() caller already tolerates rank 0),
// AutonomyRubric needs this gate: an author-facing field, so a typo must 400
// rather than silently rank as "below L0".
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

// AutonomyRubric maps a run's posture to a permitted AutonomyLevel: three
// egress postures, three secret postures, three confinement classes, each
// unset (caps nothing) or one of the four levels. A run resolves to the
// MINIMUM over every field whose posture applies
// (internal/composer/autonomy.go, #97); an all-unset rubric caps nothing,
// like a nil rubric.
//
// A closed struct with `omitempty` on every field, not a map — same
// doctrine as GovernanceLimits: small, complete, validated by the Go type
// (Validate), no DB CHECK needed.
type AutonomyRubric struct {
	// EgressOpen caps the level when egress is OPEN: allow-all, or any allowlisted host beyond baseline.
	EgressOpen AutonomyLevel `json:"egress_open,omitempty"`
	// EgressReviewed caps the level when egress is REVIEWED: first-use approval raises approvals, nothing wide open.
	EgressReviewed AutonomyLevel `json:"egress_reviewed,omitempty"`
	// EgressSealed caps the level when egress is SEALED: neither of the above.
	EgressSealed AutonomyLevel `json:"egress_sealed,omitempty"`
	// SecretsPowerful caps the level when the run holds a POWERFUL secret: a write-capable grant, an
	// api_key to a non-baseline host, or a git_pat/ssh_key/env_secret grant.
	SecretsPowerful AutonomyLevel `json:"secrets_powerful,omitempty"`
	// SecretsBaseline caps the level when the run holds any grant, none of them powerful.
	SecretsBaseline AutonomyLevel `json:"secrets_baseline,omitempty"`
	// SecretsNone caps the level when the run holds no grant at all.
	SecretsNone AutonomyLevel `json:"secrets_none,omitempty"`
	// ConfinementCC1/CC2/CC3 cap the level by the run's ENFORCED confinement
	// class (an empty enforced class reads as CC1 — see AutonomyPosture).
	ConfinementCC1 AutonomyLevel `json:"confinement_cc1,omitempty"`
	ConfinementCC2 AutonomyLevel `json:"confinement_cc2,omitempty"`
	ConfinementCC3 AutonomyLevel `json:"confinement_cc3,omitempty"`
}

// Validate reports the first field carrying an undefined AutonomyLevel,
// naming it — governanceLimitsRefusal prefixes the field name onto its 400
// so an admin knows which of the nine to fix. An empty field is always
// valid: unset caps nothing.
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
// enforced confinement class — the input resolveRunAutonomy (#97) folds
// against a profile's AutonomyRubric. Computed, never stored on its own; it
// travels inside AutonomyResolution.
type AutonomyPosture struct {
	Egress      AutonomyEgressPosture  `json:"egress"`
	Secrets     AutonomySecretsPosture `json:"secrets"`
	Confinement ConfinementClass       `json:"confinement"`
}

// AutonomyResolution is what resolveRunAutonomy (#97) decides for one run: the level, the
// posture that produced it, and every rubric field that bound the result — provenance for the
// create audit row, the frozen AgentRun.AutonomyLevel, and the preflight response.
//
// BoundBy is a LIST, deliberately: the fold is a min() over three axes, so rows routinely TIE
// at the resolved level, and naming just one cause would leave an admin editing a row that
// can't move the level while other causes stay hidden — every tied cause is named, in the
// fixed field order internal/composer/autonomy.go folds in.
//
// Empty when nothing capped the level (no profile, no rubric, or an unset posture).
type AutonomyResolution struct {
	Level   AutonomyLevel   `json:"level"`
	Posture AutonomyPosture `json:"posture"`
	BoundBy []string        `json:"bound_by,omitempty"`
}

// GovernanceProfile is one named, assignable ceiling (migration 0052's governance_profiles row).
//
// Ceiling is a full RunPolicySpec with REPLACEMENT semantics, not composition: an assigned
// profile IS the principal's ceiling; with no assignment a principal falls through to
// Config.DefaultPolicy byte for byte. composer.Clamp is NOT a lattice meet (drops
// workspace_mounts unconditionally, is order-dependent through llm_inspection, defaults
// unnamed tools to hold), so a "composed" ceiling would silently lose fields and couldn't
// express an autonomous profile.
//
// Name is the UNIQUE human handle: what an admin assigns by, what the console lists, and the
// resolver's last ORDER BY tie-break.
type GovernanceProfile struct {
	ID        uuid.UUID        `json:"id"`
	Name      string           `json:"name"`
	Ceiling   RunPolicySpec    `json:"ceiling"`
	Limits    GovernanceLimits `json:"limits"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	CreatedBy string           `json:"created_by,omitempty"`
}

// GovernanceAssignment binds one profile to one subject (migration 0052's governance_assignments row).
//
// SubjectType REUSES CapabilitySubjectType — the same vocabulary capability_grants is written
// against; a second enum for the same thing is the dual-matcher drift this codebase warns
// against elsewhere.
//
// Priority breaks ties WITHIN a tier (higher wins), the group tier's working lever since a
// member is often in several groups. Never crosses tiers: a user-tier row beats every
// group-tier row at any priority.
type GovernanceAssignment struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	ProfileID   uuid.UUID             `json:"profile_id"`
	Priority    int                   `json:"priority"`
	CreatedAt   time.Time             `json:"created_at"`
	CreatedBy   string                `json:"created_by,omitempty"`
}
