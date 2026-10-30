// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macOSHoldRefusal refuses a macOS VM run that would launch the hold lane, once autonomy and the tool
// lane have settled it (stepMacOSHold). stepPlacementCapability already refuses the holds the request and
// its policy name; a hold only autonomy level L1 or the policy's inner walls derive (req.ToolApprovals) is
// known after stepAutonomy and stepToolPolicy, so it is refused here, at create and Review, rather than by
// the guest launcher once the VM is up. It reads holdLane, the predicate dispatch sets
// WARDYN_TOOL_APPROVALS from, on the interactivity every door agrees on (requestIsInteractive).
func macOSHoldRefusal(req createRunRequest) *runRefusal {
	if !types.MachineKind(req.MachineKind).IsMacOSVM() || !holdLane(requestIsInteractive(req), req.ToolApprovals, req.TaskMode) {
		return nil
	}
	return placementRunRefusal(&placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "tools"}, macOSVMNoHold)
}
