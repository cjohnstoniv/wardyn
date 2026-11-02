// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// connectGate is the part of a CONNECT's decision that depends on the run's config alone: the policy
// host verdict, the port rule and the method restriction. evaluate (so decideConnect) runs it, and so
// does Config.TerminatedOutside, so the set of hosts a run's proxy would open is the proxy's own
// decision and not a second matcher. What it leaves out needs the live proxy: the literal-IP guard, an
// MCP or local-model host, the private-IP memo, and the DNS vetting of the target.
type connectGate struct {
	// refusal is the rule_source of the Deny the config alone gives, or "".
	refusal string
	// verdict is the evaluator's host verdict, valid when refusal is "".
	verdict egress.HostVerdict
	// portDryRun is set when the dry-run port rule would have refused the request.
	portDryRun bool
}

// gateConnect runs, in this order: the evaluator's host verdict (an error fails closed; deny beats
// allow), the port rule (sits below policy:denied and above the method check and the approval raise:
// an approval is host-wide and never opens a port, so asking for one the rule refuses is a question
// the answer cannot change), then the method restriction. The method check runs BEFORE the first-use
// approval raise because it depends on nothing the approval produces: a request whose method can
// NEVER pass must not raise an ApprovalRequest or park a hold, and since the SANDBOX picks the method
// it would otherwise pick how many approval rows and hold slots it creates, and a method-denied request
// must not spend a scope=once grant. Order against policy:denied is unchanged: a host the policy denies
// outright still logs policy:denied, never policy:method.
func gateConnect(ctx context.Context, req egress.Request, ev egress.Evaluator, pol *Policy, portRule string) connectGate {
	verdict, err := ev.EvaluateHost(ctx, req)
	if err != nil {
		return connectGate{refusal: "policy:evaluator-error"}
	}
	if verdict == egress.VerdictDeny {
		return connectGate{refusal: "policy:denied"}
	}
	g := connectGate{verdict: verdict}
	if portRuleRefuses(portRule, pol, req.Host, req.Port) {
		if portRule == PortRuleOn {
			return connectGate{refusal: ruleSourcePort}
		}
		g.portDryRun = true
	}
	if !ev.MethodAllowed(req.Method) {
		return connectGate{refusal: "policy:method"}
	}
	return g
}

// connectOutcome is the three-way answer to a CONNECT on the config alone.
type connectOutcome int

const (
	// connectRefuse is a Deny the proxy gives whatever else happens: the host is never opened.
	connectRefuse connectOutcome = iota
	// connectHold is a host the policy neither allows nor denies, and the run's first-use mode can ask
	// a person about: Pending until approved, then opened like an allowed host.
	connectHold
	// connectAdmit is a host the policy allows.
	connectAdmit
)

// outcome maps the gate to admit, hold or refuse. canAsk is whether the proxy raises a first-use
// approval for an unknown host (the mode raises one and an approval client exists).
func (g connectGate) outcome(canAsk bool) connectOutcome {
	switch {
	case g.refusal != "":
		return connectRefuse
	case g.verdict == egress.VerdictAllow:
		return connectAdmit
	case g.verdict == egress.VerdictUnknown && canAsk:
		return connectHold
	}
	return connectRefuse
}
