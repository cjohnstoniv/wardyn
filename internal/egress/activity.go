// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

// RuleSourceApprovals is the rule source of the toolgate's poll of an approval it is blocked on.
const RuleSourceApprovals = "brokered:approvals"

// CountsAsActivity reports whether a decision is the run doing something, and so moves the idle clock and
// the pause's presence clock. Only an allowed request is: a refused or held one is the run being told no,
// not the run working, and a run that is only being refused should still idle out. A model call's token
// counts (Usage) are a count and not a request, and the toolgate's poll of the approval it is blocked on is
// waiting. A re-auth hold that timed out is a deny, so it needs no rule of its own. Every ingest path that
// moves an idle clock calls this one function.
func CountsAsActivity(d DecisionLog) bool {
	return d.Decision == Allow && d.Usage == nil && d.RuleSource != RuleSourceApprovals
}

// MarkModelTunnel sets ModelTunnel on d. It does nothing on a nil d, since the CONNECT path holds no row when
// the decision was not logged.
func (d *DecisionLog) MarkModelTunnel() {
	if d != nil {
		d.ModelTunnel = true
	}
}
