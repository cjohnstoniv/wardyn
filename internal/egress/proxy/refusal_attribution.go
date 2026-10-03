// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// The headers a policy-decided refusal carries, so a sandbox can tell whose
// policy refused it and where to ask for a change. Values come from
// policyref.Ref.HeaderValues, which is ASCII.
const (
	egressHeaderPolicy        = "X-Wardyn-Policy"
	egressHeaderPolicyRequest = "X-Wardyn-Policy-Request"
)

// policyDecidedReason reports whether a decision-log reason is a refusal the
// run's policy made. Closed on purpose: a DNS or dial failure, the private-address
// guard and an evaluator error are faults, and sending someone to the policy
// owner for one costs that owner a ticket that is not theirs.
func policyDecidedReason(reason string) bool {
	switch reason {
	case "policy:denied", "policy:default-deny", "policy:method", "approval:denied":
		return true
	}
	return false
}

// reprojectAttribution re-runs policyref.Project over a decoded attribution, so
// a bad field (a request_url that no longer passes) is dropped and logged and
// the sidecar still starts. The result is the only form the proxy ever writes.
func reprojectAttribution(a *policyref.Ref) *policyref.Ref {
	if a == nil {
		return nil
	}
	ref := policyref.Project(a.Source, a.Name, &policyref.Contact{
		Owner: a.Owner, Email: a.Email, RequestURL: a.RequestURL, RequestText: a.RequestText,
	})
	if ref == nil || *ref != *a {
		slog.Warn("wardyn-proxy: config attribution dropped or trimmed by validation", slog.Bool("dropped", ref == nil))
	}
	return ref
}

// attributeRefusal sets the two policy headers on w, before the caller writes
// its status, and returns the sentence to append to the refusal's body. "" and
// no headers when the run has no attribution, which leaves the refusal exactly
// as it was. "Governed by", never "forbidden by": the proxy knows the governing
// profile, not which layer refused.
func (p *Proxy) attributeRefusal(w http.ResponseWriter) string {
	ref := p.attribution
	if ref == nil {
		return ""
	}
	policy, request := ref.HeaderValues()
	w.Header().Set(egressHeaderPolicy, policy)
	if request != "" {
		w.Header().Set(egressHeaderPolicyRequest, request)
	}
	who := "this deployment's default policy"
	if ref.Source == policyref.SourceProfile {
		who = "a governance profile"
		if ref.Name != "" {
			who = fmt.Sprintf("the profile %q", ref.Name)
		}
	}
	// The owner's name and email stay out of the body (they are a person's).
	route := request
	if route == "" {
		route = ref.RequestText
	}
	if route == "" {
		return "This run is governed by " + who + "."
	}
	return "This run is governed by " + who + ". To request a change: " + route
}

// denyAttributed is http.Error for a refusal the policy decided: msg, then the
// attribution sentence on its own line when there is one.
func (p *Proxy) denyAttributed(w http.ResponseWriter, msg string, status int) {
	if line := p.attributeRefusal(w); line != "" {
		msg += "\n" + line
	}
	http.Error(w, msg, status)
}
