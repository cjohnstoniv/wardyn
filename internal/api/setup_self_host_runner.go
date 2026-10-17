// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// selfHostGrantBody opens self-hosting to everyone: the body for POST /api/v1/permissions/grants.
const selfHostGrantBody = `{"subject_type": "all", "capability": "feature", "value": "self_host_runner", "effect": "allow"}`

// selfHostGrantCheck grades the self_host_runner_grant row: self_host_runner is restricted, runners are
// claimed, and no grant row names the value, so nobody but an admin can mint a runner token. Migration
// 0178 does not add the grant to an estate already past it, so this row is how such an estate is told.
// A deny naming the value is a choice to keep it closed and clears the row, as an allow does.
func selfHostGrantCheck(e upgradeEstate) (SetupCheck, bool) {
	if !e.SelfHostRestricted || e.ClaimedSelfHosted == 0 || e.SelfHostNamed {
		return SetupCheck{}, false
	}
	noun := "runners keep"
	if e.ClaimedSelfHosted == 1 {
		noun = "runner keeps"
	}
	return SetupCheck{ID: "self_host_runner_grant", Label: "Self-hosted runner tokens", Status: "warn",
		Detail: fmt.Sprintf("Self-hosting is restricted and no permission names self_host_runner, so people who are not admins cannot mint runner tokens "+
			"(403 capability_feature). %d claimed %s working.", e.ClaimedSelfHosted, noun),
		Fix: "To let everyone mint, POST /api/v1/permissions/grants with " + selfHostGrantBody + "; name a user, group or user_type subject " +
			"to open it to fewer people. A deny naming self_host_runner keeps it closed and clears this row. " +
			"GET /api/v1/permissions/explain shows the restriction."}, true
}
