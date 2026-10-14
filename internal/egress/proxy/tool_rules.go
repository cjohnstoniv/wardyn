// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The run's own tool_rules policy: the part of the tool-approval route that
// can answer without waking a human.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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
	eff, ok := p.toolEffect(r, tool)
	if !ok {
		return false
	}
	switch eff {
	case types.ToolAllow:
		p.emitToolDecision(r, egress.Allow, ruleSourceToolAllow, tool)
		writeToolDecision(w, "APPROVED")
		return true
	case types.ToolDeny:
		p.emitToolDecision(r, egress.Deny, ruleSourceToolDeny, tool)
		writeToolDecision(w, "DENIED")
		return true
	}
	// types.ToolHold: the human is the point. Fall through.
	return false
}

// emitToolDecision is emitLocalDecision for a call tool_rules answered. No
// approval row stands behind it, so the row's Cause names the tool, quoted
// since the name is the sandbox's own text.
func (p *Proxy) emitToolDecision(r *http.Request, decision egress.Decision, ruleSource, tool string) {
	if p.sink == nil {
		return
	}
	host, port, _ := hostPortFromURL(p.controlPlaneURL)
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.Cause = "tool " + strconv.Quote(tool)
	p.sink.emit(log)
}

// toolEffect is what the run's rules say about one call. Under per-harness rules
// the harness is the one the caller's credential names (harnessOf) and only that
// harness's rules apply: a harness with no entry has no rule and is held for a
// human, never answered by another harness's rules. A call that names no harness
// is decided by the strictest of all the sets (strictestToolEffect).
func (p *Proxy) toolEffect(r *http.Request, tool string) (types.ToolEffect, bool) {
	if len(p.harnessRules) == 0 {
		return p.policy.ToolEffectFor(tool)
	}
	harness := p.harnessOf(r)
	if harness == "" {
		return p.strictestToolEffect(tool)
	}
	return types.ToolEffectForHarness(p.harnessRules, harness, tool)
}

// strictestToolEffect is the answer for a call that names no harness (no
// credential, or one the proxy never issued): the strictest of every harness's
// set, so a deny any harness carries is a deny here and the call is never freer
// than the run-wide regime made it. A set with no opinion on the tool counts as a
// hold, which is what its own call would get.
func (p *Proxy) strictestToolEffect(tool string) (types.ToolEffect, bool) {
	rank := map[types.ToolEffect]int{types.ToolAllow: 0, types.ToolHold: 1, types.ToolDeny: 2}
	strictest := types.ToolAllow
	for _, set := range p.harnessRules {
		eff, ok := types.ToolEffectForHarness(p.harnessRules, set.Harness, tool)
		if !ok {
			eff = types.ToolHold
		}
		if rank[eff] > rank[strictest] {
			strictest = eff
		}
	}
	return strictest, len(p.harnessRules) > 0
}

// harnessOf names the harness that raised a call from the Bearer credential its
// own wardyn-toolgate sends, compared by digest in constant time. Never from the
// request body: every process in the sandbox can post to this route, so a harness
// read from the body would let one tool borrow a sibling's rules. "" is no harness.
func (p *Proxy) harnessOf(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	got := hex.EncodeToString(sum[:])
	name := ""
	for _, c := range p.harnessCreds {
		// No early exit: the loop's length must not depend on which credential matched.
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.SHA256)) == 1 {
			name = c.Harness
		}
	}
	return name
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
