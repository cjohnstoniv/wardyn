// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// The two RunSignIn.State values.
const (
	SignInWaiting    = "waiting"
	SignInNotWaiting = "not_waiting"
)

// RunSignIn is GET /api/v1/runs/{id}/sign-in: whether a running AWS sign-in
// is waiting for its device-code approval.
type RunSignIn struct {
	// State is SignInWaiting or SignInNotWaiting.
	State string `json:"state"`
	// VerificationURL is the page to approve the sign-in on, with the code
	// pre-filled; set only when waiting. It comes from the sign-in sandbox's
	// terminal, so check its host before opening it.
	VerificationURL string `json:"verification_url,omitempty"`
	// UserCode is the code the page asks for; set only when waiting.
	UserCode string `json:"user_code,omitempty"`
}

// RunSignIn reads, once, whether an AWS sign-in run is waiting for its
// device-code approval (the run's owner only; a super admin on a person's run
// gets 403 run_owner_only). 409 run_sign_in_not_aws for any other run; 503
// run_sign_in_unreadable when the sandbox's terminal could not be read in time.
func (c *Client) RunSignIn(ctx context.Context, runID uuid.UUID) (RunSignIn, error) {
	var out RunSignIn
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/sign-in", nil, &out)
	return out, err
}
