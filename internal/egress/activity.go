// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import "github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"

// RuleSourceApprovals is the rule source of the toolgate's poll of an approval it is blocked on.
const RuleSourceApprovals = "brokered:approvals"

// RuleSourceUpstreamProxy is the rule source of the one allow row a proxy writes when it starts with an
// upstream proxy: it records the private-IP guard's relaxation for that hop, not a request.
const RuleSourceUpstreamProxy string = "builtin:upstream-proxy"

// CountsAsActivity reports whether a decision is the run doing something, and so moves the idle clock and
// the pause's presence clock. Only an allowed request is: a refused or held one is the run being told no,
// not the run working, and a run that is only being refused should still idle out. A model call's token
// counts (Usage) are a count and not a request, and the toolgate's poll of the approval it is blocked on is
// waiting. The upstream proxy's start-up row (RuleSourceUpstreamProxy) records configuration, and a via_org
// engine writes it again on every rebuild. A request that arrived on a VM run's relay listener (Request.Relayed) is a guest flow that ignored the
// run's proxy settings, the guest OS's own services, and is not the run working either. A re-auth hold that timed out is a deny, so it needs no rule of its own. Every ingest path that
// moves an idle clock calls this one function.
func CountsAsActivity(d DecisionLog) bool {
	return d.Decision == Allow && d.Usage == nil && d.RuleSource != RuleSourceApprovals &&
		d.RuleSource != RuleSourceUpstreamProxy && !d.Request.Relayed
}

// CountsAsActivityFor is CountsAsActivity less the allowed requests to a host on ignore, matched like the
// policy's domain lists (an exact host, or a "*." wildcard). A macOS guest calls Apple's boot hosts in the
// background, and a run that allows them would otherwise read that traffic as its own work. A nil ignore
// changes nothing. The control plane and the run's proxy both judge activity through this function.
func CountsAsActivityFor(d DecisionLog, ignore []string) bool {
	if !CountsAsActivity(d) {
		return false
	}
	host := domainmatch.CanonHost(d.Request.Host)
	for _, entry := range ignore {
		exact, wild, _ := domainmatch.Classify(entry)
		if exact == host || (wild != "" && domainmatch.MatchWild(host, []string{wild})) {
			return false
		}
	}
	return true
}

// MarkModelTunnel sets ModelTunnel on d. It does nothing on a nil d, since the CONNECT path holds no row when
// the decision was not logged.
func (d *DecisionLog) MarkModelTunnel() {
	if d != nil {
		d.ModelTunnel = true
	}
}
