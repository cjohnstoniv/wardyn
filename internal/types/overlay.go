// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// CeilingOverlay is the part of a RunPolicySpec a composed governance profile
// narrows, with PRESENCE typed: every field is a pointer, so "the overlay says
// nothing about this" (nil, inherit the base) and "the overlay says this is
// empty" (a pointer to an empty list, a value) can never be confused. That
// distinction is load-bearing: allowed_domains [] narrows to no domains, while
// allowed_methods [] would read as "all methods" in the proxy and is refused.
//
// It mirrors RunPolicySpec field for field (composer's coverage guard fails on
// a field of either that the other lacks). Decoded strictly: an unknown key is
// an error, never a silently ignored narrowing.
type CeilingOverlay struct {
	AllowedDomains          *[]string              `json:"allowed_domains,omitempty"`
	DeniedDomains           *[]string              `json:"denied_domains,omitempty"`
	AllowAllEgress          *bool                  `json:"allow_all_egress,omitempty"`
	FirstUseApproval        *FirstUseMode          `json:"first_use_approval,omitempty"`
	FirstUseHoldSeconds     *int                   `json:"first_use_hold_seconds,omitempty"`
	MaxHolds                *int                   `json:"max_holds,omitempty"`
	AllowedMethods          *[]string              `json:"allowed_methods,omitempty"`
	MinConfinementClass     *ConfinementClass      `json:"min_confinement_class,omitempty"`
	EligibleGrants          *[]GrantSpec           `json:"eligible_grants,omitempty"`
	AutoStopAfterSec        *int                   `json:"auto_stop_after_sec,omitempty"`
	WorkspaceMounts         *[]WorkspaceMount      `json:"workspace_mounts,omitempty"`
	WorkspaceRepos          *[]WorkspaceRepo       `json:"workspace_repos,omitempty"`
	LLMInspection           *LLMInspectionSpec     `json:"llm_inspection,omitempty"`
	UIApps                  *[]UIApp               `json:"ui_apps,omitempty"`
	Resources               *ResourcesOverlay      `json:"resources,omitempty"`
	ToolRules               *[]ToolRule            `json:"tool_rules,omitempty"`
	GitPushAnyBranch        *bool                  `json:"git_push_any_branch,omitempty"`
	PushRules               *PushRulesOverlay      `json:"push_rules,omitempty"`
	AzureDevOpsCapabilities *[]adoscope.Capability `json:"azure_devops_capabilities,omitempty"`
	GitHubCapabilities      *[]ghscope.Capability  `json:"github_capabilities,omitempty"`
}

// ResourcesOverlay mirrors ResourceLimits with per-field presence.
type ResourcesOverlay struct {
	CPUMillis *int `json:"cpu_millis,omitempty"`
	MemoryMiB *int `json:"memory_mib,omitempty"`
	PidsLimit *int `json:"pids_limit,omitempty"`
	DiskMiB   *int `json:"disk_mib,omitempty"`
}

// PushRulesOverlay mirrors PushRulesSpec with per-field presence.
type PushRulesOverlay struct {
	DenyPaths          *[]string `json:"deny_paths,omitempty"`
	MaxInspectPackMiB  *int      `json:"max_inspect_pack_mib,omitempty"`
	RequireReviewPaths *[]string `json:"require_review_paths,omitempty"`
	HoldSeconds        *int      `json:"hold_seconds,omitempty"`
	DenyNewExecutables *bool     `json:"deny_new_executables,omitempty"`
	MaxFileSizeMiB     *int      `json:"max_file_size_mib,omitempty"`
}

// LimitsOverlay is CeilingOverlay's counterpart for GovernanceLimits. The
// embedded RunLimits fields sit flat on the wire, as they do on the limits
// object itself.
type LimitsOverlay struct {
	DenyTaskModeExec    *bool           `json:"deny_task_mode_exec,omitempty"`
	DenyInteractive     *bool           `json:"deny_interactive,omitempty"`
	DenyUIApps          *bool           `json:"deny_ui_apps,omitempty"`
	MaxConcurrentRuns   *int            `json:"max_concurrent_runs,omitempty"`
	DenyUserDrive       *bool           `json:"deny_user_drive,omitempty"`
	MaxCPUMillis        *int            `json:"max_cpu_millis,omitempty"`
	MaxMemoryMiB        *int            `json:"max_memory_mib,omitempty"`
	MaxEphemeralDiskMiB *int            `json:"max_ephemeral_disk_mib,omitempty"`
	MaxDriveSizeMiB     *int            `json:"max_drive_size_mib,omitempty"`
	AutonomyRubric      *AutonomyRubric `json:"autonomy_rubric,omitempty"`
	MaxEndAheadSec      *int            `json:"max_end_ahead_sec,omitempty"`
	DefaultEndSec       *int            `json:"default_end_sec,omitempty"`
	AllowNoEnd          *bool           `json:"allow_no_end,omitempty"`
	MaxWaitSec          *int            `json:"max_wait_sec,omitempty"`
	DefaultWaitSec      *int            `json:"default_wait_sec,omitempty"`
	UserChangesLimits   *bool           `json:"user_changes_limits,omitempty"`
	PauseIdleAfterSec   *int            `json:"pause_idle_after_sec,omitempty"`
}

// DecodeCeilingOverlay strictly decodes a stored or submitted overlay.
func DecodeCeilingOverlay(raw []byte) (CeilingOverlay, error) {
	var o CeilingOverlay
	return o, decodeOverlayStrict(raw, &o)
}

// DecodeLimitsOverlay strictly decodes a stored or submitted limits overlay.
func DecodeLimitsOverlay(raw []byte) (LimitsOverlay, error) {
	var o LimitsOverlay
	return o, decodeOverlayStrict(raw, &o)
}

// decodeOverlayStrict refuses unknown fields and anything after the one object.
func decodeOverlayStrict(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("overlay: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("overlay: unexpected data after the JSON object")
	}
	return nil
}
