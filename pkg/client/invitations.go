// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// CreateInvitationRequest is the POST /api/v1/people/{principal}/invitations body; every field is
// optional. internal/api aliases it (createInvitationRequest), so the strict server decode and this
// struct are one type.
type CreateInvitationRequest struct {
	// ExpiresInSeconds is how long the invitation stays claimable; zero asks for the default (72h), at most 14d.
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
	// TokenName names the token the claim creates; the claimer may change it.
	TokenName string `json:"token_name,omitempty"`
	// TokenTTLSeconds limits the claimed token's lifetime; zero leaves it to the deployment's rule.
	TokenTTLSeconds int64 `json:"token_ttl_seconds,omitempty"`
	// ReplacesTokenID is a live token an admin minted for the target, revoked when the target claims.
	ReplacesTokenID *uuid.UUID `json:"replaces_token_id,omitempty"`
}

// ClaimInvitationRequest is the POST /api/v1/me/invitations/{id}/claim body. An empty Name takes the
// invitation's token name, and a zero TTLSeconds its limit, else the deployment's rule.
// internal/api aliases it (claimInvitationRequest).
type ClaimInvitationRequest struct {
	Name       string `json:"name"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// CreateInvitation invites one known person to mint their own API token (security tier). The answer
// is metadata and holds no secret: only the target, signed in, can claim it.
// POST /api/v1/people/{principal}/invitations.
func (c *Client) CreateInvitation(ctx context.Context, principal string, req CreateInvitationRequest) (Invitation, error) {
	var out Invitation
	err := c.do(ctx, http.MethodPost, "/api/v1/people/"+url.PathEscape(principal)+"/invitations", req, &out)
	return out, err
}

// ListInvitations lists every person's invitations, newest first (security tier). state is
// "pending", "claimed", "expired" or "revoked"; empty keeps all. GET /api/v1/invitations.
func (c *Client) ListInvitations(ctx context.Context, state InvitationState, opts ...ListOpts) ([]Invitation, error) {
	out, _, err := c.listInvitations(ctx, "/api/v1/invitations", state, opts)
	return out, err
}

// ListPersonInvitations lists one person's invitations (security tier). GET /api/v1/people/{principal}/invitations.
func (c *Client) ListPersonInvitations(ctx context.Context, principal string, state InvitationState, opts ...ListOpts) ([]Invitation, error) {
	out, _, err := c.ListPersonInvitationsPage(ctx, principal, state, opts...)
	return out, err
}

// ListPersonInvitationsPage is ListPersonInvitations plus the server's X-Wardyn-Truncated signal.
func (c *Client) ListPersonInvitationsPage(ctx context.Context, principal string, state InvitationState, opts ...ListOpts) (invitations []Invitation, truncated bool, err error) {
	return c.listInvitations(ctx, "/api/v1/people/"+url.PathEscape(principal)+"/invitations", state, opts)
}

// ListMyInvitations lists the caller's own pending invitations (a signed-in person).
// GET /api/v1/me/invitations.
func (c *Client) ListMyInvitations(ctx context.Context, opts ...ListOpts) ([]Invitation, error) {
	out, _, err := c.listInvitations(ctx, "/api/v1/me/invitations", "", opts)
	return out, err
}

func (c *Client) listInvitations(ctx context.Context, path string, state InvitationState, opts []ListOpts) (out []Invitation, truncated bool, err error) {
	if state != "" {
		path += "?state=" + url.QueryEscape(string(state))
	}
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts(path, opts), nil, &out, &hdr)
	return out, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// RevokeInvitation revokes a pending invitation (security tier). One that is missing, claimed, expired
// or already revoked answers the same 404. DELETE /api/v1/invitations/{id}.
func (c *Client) RevokeInvitation(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/invitations/"+id.String(), nil, nil)
}

// ClaimInvitation claims the caller's own invitation and returns the new token; Token holds the
// plaintext, which the server shows once. It needs the target's signed-in browser session, so a bearer
// token cannot claim: the CLI has no verb for it. POST /api/v1/me/invitations/{id}/claim.
func (c *Client) ClaimInvitation(ctx context.Context, id uuid.UUID, req ClaimInvitationRequest) (APIToken, error) {
	var out APIToken
	err := c.do(ctx, http.MethodPost, "/api/v1/me/invitations/"+id.String()+"/claim", req, &out)
	return out, err
}
