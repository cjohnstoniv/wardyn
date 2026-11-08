// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// Restriction is a list of narrowing rules that follows people (migration 0227): it applies to every
// governed run of the people it covers, in every unit they run in, and never counts as a context. It can
// only narrow, so its spec and limits hold the knobs a unit's walls compose by a narrow-only meet (a list
// it intersects, a rule it makes stricter, a floor it raises) and nothing else.
type Restriction struct {
	ID        uuid.UUID           `json:"id"`
	Name      string              `json:"name"`
	Spec      RestrictionSpec     `json:"spec"`
	Limits    RestrictionLimits   `json:"limits"`
	Contact   *policyref.Contact  `json:"contact,omitempty"`
	Members   []RestrictionMember `json:"members"`
	CreatedBy string              `json:"created_by,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// RestrictionMember is one subject a restriction covers: a user, a group, a user type, or everyone.
type RestrictionMember struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
}

// RestrictionWrite is the body of POST and PUT /governance/restrictions: the whole restriction but
// the server-set fields. A PUT replaces the members too.
type RestrictionWrite struct {
	Name    string              `json:"name"`
	Spec    RestrictionSpec     `json:"spec"`
	Limits  RestrictionLimits   `json:"limits"`
	Contact *policyref.Contact  `json:"contact,omitempty"`
	Members []RestrictionMember `json:"members"`
}

// RestrictionSpec is the part of a RunPolicySpec a restriction may carry. Presence is typed, as in a
// composed unit's overlay: a knob it does not name is no limit. A bool can only be false, since a
// true would allow what a unit's walls may deny. Each knob composes as the same knob of a unit's walls.
type RestrictionSpec struct {
	DeniedDomains       *[]string         `json:"denied_domains,omitempty"`
	AllowAllEgress      *bool             `json:"allow_all_egress,omitempty"`
	FirstUseApproval    *FirstUseMode     `json:"first_use_approval,omitempty"`
	FirstUseHoldSeconds *int              `json:"first_use_hold_seconds,omitempty"`
	MaxHolds            *int              `json:"max_holds,omitempty"`
	MinConfinementClass *ConfinementClass `json:"min_confinement_class,omitempty"`
	AutoStopAfterSec    *int              `json:"auto_stop_after_sec,omitempty"`
	GitPushAnyBranch    *bool             `json:"git_push_any_branch,omitempty"`
	// AllowedDomains narrows egress to these hosts. Named without allow_all_egress, it also turns open
	// egress off for the restriction's people, as the boundary's allowlist does: an allowlist bounds
	// nothing while all egress is open.
	AllowedDomains *[]string         `json:"allowed_domains,omitempty"`
	ToolRules      *[]ToolRule       `json:"tool_rules,omitempty"`
	PushRules      *PushRulesOverlay `json:"push_rules,omitempty"`
	Desktop        *DesktopControls  `json:"desktop,omitempty"`
	// Recording is a floor: a run of the restriction's people is recorded at least this much.
	Recording *RecordingLevel `json:"recording,omitempty"`
	// DataOut is a floor on the rule for content leaving a run of the restriction's people: the stricter of the
	// two applies (allowed, review or blocked; docs/POLICIES.md#data_out).
	DataOut *TransferRule `json:"data_out,omitempty"`
	// WithheldGrants names credentials the run is not given: a grant of the same kind (a scope-less
	// entry) or the same pairing is kept from it at dispatch and mint. It never reaches Overlay().
	WithheldGrants []GrantSpec `json:"withheld_grants,omitempty"`
	// AzureDevOpsCapabilities caps a restriction's people's Azure DevOps capabilities: a capability it
	// does not list is refused at dispatch, at each request, at each approval and at each credential
	// resolve, whatever a provider row allows. pr reads as its parts. nil is no cap; an empty list is
	// refused at write. Like WithheldGrants it never reaches Overlay(): a list meet keeps a run on the
	// provider's default profile as it is, so the cap is met with the row's ceiling instead.
	AzureDevOpsCapabilities *[]adoscope.Capability `json:"azure_devops_capabilities,omitempty"`
	// GitHubCapabilities is AzureDevOpsCapabilities' GitHub twin, applied to the GitHub CLI's
	// capabilities and to git pushes (code_write) at launch.
	GitHubCapabilities *[]ghscope.Capability `json:"github_capabilities,omitempty"`
}

// RestrictionLimits is the part of GovernanceLimits a restriction may carry. The six deny_* switches
// only turn on; the other bools can only be false.
type RestrictionLimits struct {
	LocalSelfDefinedComponents *bool        `json:"local_self_defined_components,omitempty"`
	DenyTaskModeExec           *bool        `json:"deny_task_mode_exec,omitempty"`
	DenyInteractive            *bool        `json:"deny_interactive,omitempty"`
	DenyUIApps                 *bool        `json:"deny_ui_apps,omitempty"`
	DenyBackgroundDesktopWatch *bool        `json:"deny_background_desktop_watch,omitempty"`
	DenyUserDrive              *bool        `json:"deny_user_drive,omitempty"`
	DenyMemberDrives           *bool        `json:"deny_member_drives,omitempty"`
	DenyKeptMachines           *bool        `json:"deny_kept_machines,omitempty"`
	DenyScheduledRuns          *bool        `json:"deny_scheduled_runs,omitempty"`
	DenyComponents             *[]string    `json:"deny_components,omitempty"`
	DenyModelProviders         *[]string    `json:"deny_model_providers,omitempty"`
	DenyPlacements             *[]Placement `json:"deny_placements,omitempty"`
	AllowedMacOSBases          *[]string    `json:"allowed_macos_bases,omitempty"`
	RequiredImageSigners       *[]string    `json:"required_image_signers,omitempty"`
	AllowedModels              *[]string    `json:"allowed_models,omitempty"`
	MaxConcurrentRuns          *int         `json:"max_concurrent_runs,omitempty"`
	MaxCPUMillis               *int         `json:"max_cpu_millis,omitempty"`
	MaxMemoryMiB               *int         `json:"max_memory_mib,omitempty"`
	MaxEphemeralDiskMiB        *int         `json:"max_ephemeral_disk_mib,omitempty"`
	MaxDriveSizeMiB            *int         `json:"max_drive_size_mib,omitempty"`
	MaxEndAheadSec             *int         `json:"max_end_ahead_sec,omitempty"`
	MaxWaitSec                 *int         `json:"max_wait_sec,omitempty"`
	PauseIdleAfterSec          *int         `json:"pause_idle_after_sec,omitempty"`
	AllowNoEnd                 *bool        `json:"allow_no_end,omitempty"`
	UserChangesLimits          *bool        `json:"user_changes_limits,omitempty"`
}

// RestrictionBoolsFalseOnly is the write-time rule that a restriction only turns a permission off:
// allow_all_egress, git_push_any_branch, local_self_defined_components, allow_no_end and
// user_changes_limits may be absent or false, never true.
func RestrictionBoolsFalseOnly(sp RestrictionSpec, l RestrictionLimits) error {
	for _, k := range []struct {
		name string
		v    *bool
	}{
		{"allow_all_egress", sp.AllowAllEgress},
		{"git_push_any_branch", sp.GitPushAnyBranch},
		{"local_self_defined_components", l.LocalSelfDefinedComponents},
		{"allow_no_end", l.AllowNoEnd},
		{"user_changes_limits", l.UserChangesLimits},
	} {
		if k.v != nil && *k.v {
			return fmt.Errorf("a restriction can only turn %s off", k.name)
		}
	}
	return nil
}

// Overlay is the spec as a ceiling overlay. A restriction that names an allowlist closes open egress, as
// the boundary's does, and one that names neither an allowlist nor allow_all_egress must not close it by
// naming nothing, so allow_all_egress is then true; the meet is an AND, so true narrows nothing.
func (s RestrictionSpec) Overlay() CeilingOverlay {
	out := CeilingOverlay{
		DeniedDomains: s.DeniedDomains, AllowAllEgress: s.AllowAllEgress, FirstUseApproval: s.FirstUseApproval,
		FirstUseHoldSeconds: s.FirstUseHoldSeconds, MaxHolds: s.MaxHolds, MinConfinementClass: s.MinConfinementClass,
		AutoStopAfterSec: s.AutoStopAfterSec, GitPushAnyBranch: s.GitPushAnyBranch,
		AllowedDomains: s.AllowedDomains, ToolRules: s.ToolRules, PushRules: s.PushRules, Desktop: s.Desktop, Recording: s.Recording, DataOut: s.DataOut,
	}
	switch {
	case out.AllowAllEgress != nil:
	case s.AllowedDomains != nil:
		off := false // an allowlist bounds nothing while all egress is open (as the boundary's)
		out.AllowAllEgress = &off
	default:
		on := true // a layer that only denies must not close open egress by naming nothing
		out.AllowAllEgress = &on
	}
	return out
}

// Overlay is the limits as a limits overlay.
func (l RestrictionLimits) Overlay() LimitsOverlay {
	return LimitsOverlay{
		LocalSelfDefinedComponents: l.LocalSelfDefinedComponents, DenyTaskModeExec: l.DenyTaskModeExec,
		DenyInteractive: l.DenyInteractive, DenyUIApps: l.DenyUIApps, DenyBackgroundDesktopWatch: l.DenyBackgroundDesktopWatch,
		DenyUserDrive: l.DenyUserDrive, DenyMemberDrives: l.DenyMemberDrives, DenyKeptMachines: l.DenyKeptMachines, DenyScheduledRuns: l.DenyScheduledRuns, DenyComponents: l.DenyComponents, DenyModelProviders: l.DenyModelProviders,
		DenyPlacements: l.DenyPlacements, AllowedMacOSBases: l.AllowedMacOSBases, RequiredImageSigners: l.RequiredImageSigners, AllowedModels: l.AllowedModels,
		MaxConcurrentRuns: l.MaxConcurrentRuns, MaxCPUMillis: l.MaxCPUMillis, MaxMemoryMiB: l.MaxMemoryMiB,
		MaxEphemeralDiskMiB: l.MaxEphemeralDiskMiB, MaxDriveSizeMiB: l.MaxDriveSizeMiB,
		MaxEndAheadSec: l.MaxEndAheadSec, MaxWaitSec: l.MaxWaitSec, PauseIdleAfterSec: l.PauseIdleAfterSec,
		AllowNoEnd: l.AllowNoEnd, UserChangesLimits: l.UserChangesLimits,
	}
}
