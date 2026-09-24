// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "time"

// defaultApprovalExpiryAfter mirrors cmd/wardynd's own approval-expiry-after
// sweeper default (boot_flags.go), so a Config built without wardynd's flags
// (every test harness, notably) hands a hold-mode run the same ceiling
// production does.
const defaultApprovalExpiryAfter = 24 * time.Hour

// approvalExpiryCeiling is the ceiling dispatch mirrors onto a hold-mode run's
// sandbox (WARDYN_APPROVAL_EXPIRY_AFTER, applyDispatchModeEnv): the SAME one the
// sweeper expires a PENDING approval at, including a tool_call hold, so
// wardyn-toolgate's -deadline default and agent-run's MCP_TOOL_TIMEOUT track an
// operator-raised value instead of their own hardcoded literal (RL-1). Unset
// (Config.ApprovalExpiryAfter <= 0, which captureRunLimits reads as "unknown")
// falls back to the sweeper's default here only.
func approvalExpiryCeiling(configured time.Duration) time.Duration {
	if configured <= 0 {
		return defaultApprovalExpiryAfter
	}
	return configured
}
