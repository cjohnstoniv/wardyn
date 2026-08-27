// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The run's own tool_rules policy: the part of the tool-approval route that can
// answer without waking a human.
//
// Its own file rather than more of local_routes.go, which routes six unrelated
// brokered lanes and sits against the 1000-line gate. This is one question —
// may this tool call proceed, and who decided — so it reads better alone.

import (
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decideByToolRules applies the run's tool_rules to one tool call, reporting
// whether it answered the request.
//
// EVALUATED PROXY-SIDE, outside the sandbox, on the policy the control plane
// resolved — so a compromised agent cannot rewrite the rules that govern it.
//
// An `allow` or a `deny` is answered immediately and NO approval row is
// created: waking a human for a call the operator already decided is exactly the
// approval fatigue that makes people set tool_approvals=auto for everything and
// give up on gating entirely. Both outcomes still hit the decision log, so
// "policy waved this through" is as visible in audit as "a human approved it".
//
// Returns false — leaving the caller to raise a human approval, unchanged — for
// `hold`, and for a run with no rules at all. That fallback is what makes the
// field additive: a policy authored before tool_rules existed behaves exactly as
// it did.
func (p *Proxy) decideByToolRules(w http.ResponseWriter, r *http.Request, tool string) bool {
	eff, ok := p.policy.ToolEffectFor(tool)
	if !ok {
		return false
	}
	switch eff {
	case types.ToolAllow:
		p.emitLocalDecision(r, egress.Allow, ruleSourceToolAllow, nil)
		writeToolDecision(w, "APPROVED")
		return true
	case types.ToolDeny:
		p.emitLocalDecision(r, egress.Deny, ruleSourceToolDeny, nil)
		writeToolDecision(w, "DENIED")
		return true
	}
	// types.ToolHold: the human is the point. Fall through.
	return false
}

// writeToolDecision answers a tool-approval request the run's own policy already
// decided, without creating an approval row for a human.
//
// The response carries a TERMINAL `state` and no `id`. wardyn-toolgate reads
// `state` first and requires an id only when it is absent, so a policy decision
// short-circuits the poll loop — and a gate built before tool_rules existed is
// unaffected, because the no-rule path never produces this response.
func writeToolDecision(w http.ResponseWriter, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"state":      state,
		"decided_by": "policy",
	})
}
