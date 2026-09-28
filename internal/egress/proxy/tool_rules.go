// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The run's own tool_rules policy: the part of the tool-approval route that
// can answer without waking a human.

import (
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decideByToolRules applies the run's tool_rules to one tool call, reporting
// whether it answered the request. Evaluated proxy-side, outside the sandbox,
// so a compromised agent cannot rewrite the rules governing it. `allow`/`deny`
// answer immediately with no approval row created, but still hit the decision
// log; `hold`, and a run with no rules at all, return false so the caller
// raises a human approval unchanged.
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

// writeToolDecision answers a tool-approval request the policy already
// decided, without an approval row. The response carries a terminal `state`
// and no `id`, which wardyn-toolgate reads as short-circuiting its poll loop.
func writeToolDecision(w http.ResponseWriter, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"state":      state,
		"decided_by": "policy",
	})
}
