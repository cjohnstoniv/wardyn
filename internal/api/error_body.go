// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
	// PendingChange names the held governance change a 409 governance_change_pending is ABOUT.
	// Absent when the change was decided between the refusal and the read.
	PendingChange *types.GovernanceChange `json:"pending_change,omitempty"`
	// PendingChangeMatches is present with PendingChange: true when the held change is this very
	// proposal (same op, same payload), so a repeat apply can report it as still pending; false when
	// it is another change, which the repeat must fail on rather than report as its own.
	PendingChangeMatches *bool `json:"pending_change_matches,omitempty"`
}

// writeErrorReasonPolicy is writeErrorReason for a ceiling refusal that names the
// policy bound the person (nil ref: the body writeErrorReason would write).
func writeErrorReasonPolicy(w http.ResponseWriter, status int, reason, msg string, ref *policyref.Ref) {
	writeJSON(w, status, errorBody{Error: msg, Reason: reason, Policy: ref})
}
