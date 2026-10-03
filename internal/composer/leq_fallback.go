// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Leq's last door. Each rule in leq.go compares the fields it covers; whatever
// those rules do not cover must be identical on both sides, or Leq cannot prove
// narrowing and answers false. A field added to RunPolicySpec or
// GovernanceLimits without a rule therefore fails closed at runtime, as well as
// failing the coverage guard at test time.

// equalOutside reports whether a and b are equal once covered has cleared every
// field a Leq rule handles.
func equalOutside[T any](a, b T, covered func(T) T) bool {
	return reflect.DeepEqual(covered(a), covered(b))
}

func equalOutsideCeilingRules(a, b types.RunPolicySpec) bool {
	return equalOutside(a, b, withoutCeilingRules)
}

func equalOutsideLimitRules(a, b types.GovernanceLimits) bool {
	return equalOutside(a, b, withoutLimitRules)
}

// withoutCeilingRules clears every RunPolicySpec field Leq has a rule for.
func withoutCeilingRules(s types.RunPolicySpec) types.RunPolicySpec {
	s.AllowedDomains, s.DeniedDomains, s.AllowAllEgress = nil, nil, false
	s.FirstUseApproval, s.FirstUseHoldSeconds, s.MaxHolds = "", 0, 0
	s.AllowedMethods, s.MinConfinementClass, s.EligibleGrants = nil, "", nil
	s.AutoStopAfterSec, s.WorkspaceMounts, s.WorkspaceRepos = 0, nil, nil
	s.LLMInspection, s.UIApps, s.Resources, s.ToolRules = nil, nil, nil, nil
	s.GitPushAnyBranch, s.PushRules, s.AzureDevOpsCapabilities = false, nil, nil
	return s
}

// withoutLimitRules clears every GovernanceLimits field Leq has a rule for.
func withoutLimitRules(l types.GovernanceLimits) types.GovernanceLimits {
	l.DenyTaskModeExec, l.DenyInteractive, l.DenyUIApps, l.DenyUserDrive = false, false, false, false
	l.MaxConcurrentRuns, l.MaxCPUMillis, l.MaxMemoryMiB = 0, 0, 0
	l.MaxEphemeralDiskMiB, l.MaxDriveSizeMiB, l.AutonomyRubric = 0, 0, nil
	l.MaxEndAheadSec, l.DefaultEndSec, l.AllowNoEnd = 0, 0, false
	l.MaxWaitSec, l.DefaultWaitSec, l.UserChangesLimits, l.PauseIdleAfterSec = 0, 0, false, 0
	return l
}
