// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// errorBody is the uniform JSON error envelope.
type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"` // machine-readable refusal class; the SDK exposes it as APIError.Reason; coverage phased in under #204
	// Provider and Kind (#532) name the model provider a run.create/Review 422
	// is ABOUT, so the console can open the door of that provider instead of
	// guessing from the roster. Present only on a model-provider refusal whose
	// provider is known — never on an unrelated 4xx.
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Policy (deny-f2) names the policy whose ceiling refused the request and how
	// to ask for a change. Present only on a ceiling refusal that carries one.
	Policy *policyref.Ref `json:"policy,omitempty"`
}

// writeErrorReasonPolicy is writeErrorReason for a ceiling refusal that names the
// policy bound the person (nil ref: the body writeErrorReason would write).
func writeErrorReasonPolicy(w http.ResponseWriter, status int, reason, msg string, ref *policyref.Ref) {
	writeJSON(w, status, errorBody{Error: msg, Reason: reason, Policy: ref})
}
