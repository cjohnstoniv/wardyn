// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// RunnerRegistration is the public registration identity, without owner facts or private key.
type RunnerRegistration = types.RunnerRegistration

// RunnerRegisterRequest submits a one-use token and the runner's public key.
type RunnerRegisterRequest = types.RunnerRegisterRequest

// RunnerRegistrationToken is returned once at mint.
type RunnerRegistrationToken = types.RunnerRegistrationToken

// MintRunnerToken mints for the caller when owner is empty, or for a named person (operator only).
func (c *Client) MintRunnerToken(ctx context.Context, owner string) (RunnerRegistrationToken, error) {
	var out RunnerRegistrationToken
	path := "/api/v1/me/runners/tokens"
	var body any = struct{}{}
	if owner != "" {
		path = "/api/v1/runners/tokens"
		body = types.RunnerTokenRequest{Owner: owner}
	}
	err := c.do(ctx, http.MethodPost, path, body, &out)
	return out, err
}

// RegisterRunner exchanges a single-use token for an unclaimed runner identity.
func (c *Client) RegisterRunner(ctx context.Context, req RunnerRegisterRequest) (RunnerRegistration, error) {
	var out RunnerRegistration
	err := c.do(ctx, http.MethodPost, "/api/v1/runners/register", req, &out)
	return out, err
}

// ClaimRunner confirms the locally observed fingerprint under the owner's personal session.
func (c *Client) ClaimRunner(ctx context.Context, id uuid.UUID, fingerprint string) (RunnerRegistration, error) {
	var out RunnerRegistration
	err := c.do(ctx, http.MethodPost, "/api/v1/me/runners/"+id.String()+"/claim", types.RunnerClaimRequest{Fingerprint: fingerprint}, &out)
	return out, err
}

// MintOrgRunnerToken mints a single-use, 72 h token for an organization's runner (a security operator).
// name labels the token in its audit row.
func (c *Client) MintOrgRunnerToken(ctx context.Context, name string) (RunnerRegistrationToken, error) {
	var out RunnerRegistrationToken
	err := c.do(ctx, http.MethodPost, "/api/v1/runners/org-tokens", types.RunnerOrgTokenRequest{Name: name}, &out)
	return out, err
}

// ClaimOrgRunner confirms an organization runner's fingerprint under a security operator's personal session.
func (c *Client) ClaimOrgRunner(ctx context.Context, id uuid.UUID, fingerprint string) (RunnerRegistration, error) {
	var out RunnerRegistration
	err := c.do(ctx, http.MethodPost, "/api/v1/runners/"+id.String()+"/claim-org", types.RunnerClaimRequest{Fingerprint: fingerprint}, &out)
	return out, err
}

// SetRunnerTrust changes a runner's trust class (a security operator), with the reason the audit keeps. A
// change held for a second human returns *PendingApprovalError.
func (c *Client) SetRunnerTrust(ctx context.Context, id uuid.UUID, class types.RunnerTrustClass, reason string) (RunnerView, error) {
	var out RunnerView
	err := c.do(ctx, http.MethodPut, "/api/v1/runners/"+id.String()+"/trust", types.RunnerTrustRequest{TrustClass: class, Reason: reason}, &out)
	return out, err
}
