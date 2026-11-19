// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// llmHostDenied refuses a brokered LLM request whose upstream host the run's policy denies, and
// reports whether it did. The broker dials that host for the sandbox, with or without a credential
// (a local model with no auth gets none), so a deny reaches it as it reaches a CONNECT: a profile's
// deny, or a live egress edit's. Only an explicit deny stops it here; an upstream the allowlist does
// not name (a control-plane-configured gateway) is the broker's to dial, as before.
func (p *Proxy) llmHostDenied(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	verdict, err := p.evaluator.EvaluateHost(r.Context(), p.reqOf(r, host, port))
	if err == nil && verdict != egress.VerdictDeny {
		return false
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Deny, "policy:denied")
	if p.sink != nil {
		p.sink.emit(log)
	}
	p.writeEgressDenyLLM(w, p.shapeOf(r, host), host, port, &log)
	return true
}
