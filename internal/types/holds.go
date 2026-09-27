// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// The three hold windows approval.Hold ports from ui/src/app/lib/types/
// approvals.ts's now-retired isHeld — see that function's own history (#725/
// F1, #509, #181) for why each window is what it is. Exported here, the one
// package both internal/approval and internal/egress/proxy already import,
// so the proxy's own defaultHoldTimeout/maxHoldTimeout/maxCapabilityHoldTimeout
// point at these instead of carrying a second copy of the same numbers
// (#1197).
const (
	// HoldWindowEgress is how long an egress_domain wait_for_review request
	// holds the sandbox from when it was raised (proxy's defaultHoldTimeout).
	// This under-reports a policy authored above 30s (first_use_hold_seconds,
	// up to maxHoldTimeout) — ported verbatim from the TS rule it replaces;
	// fixing the under-report is a separate proxy change (the proxy would
	// need to stamp its own resolved hold_seconds into the raise scope),
	// not this one.
	HoldWindowEgress = 30 * time.Second
	// HoldWindowADOCapability is how long an Azure DevOps capability
	// escalation (a tool_call with grant_id set) holds the sandbox (proxy's
	// maxCapabilityHoldTimeout).
	HoldWindowADOCapability = 240 * time.Second
	// HoldWindowPush is the proxy's absolute ceiling on a push_content hold
	// (maxHoldTimeout) — the conservative bound isHeld used when no client
	// surface reads a run's own resolved push_rules.hold_seconds.
	HoldWindowPush = 600 * time.Second
)
