// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// CreateServiceIdentityRequest is the POST /api/v1/service-identities body. internal/api aliases it
// (serviceIdentityRequest), so the strict server decode and this struct are one type.
type CreateServiceIdentityRequest struct {
	// Name is "^[a-z][a-z0-9-]{2,62}$"; the identity's principal is "service:<name>".
	Name        string `json:"name"`
	Description string `json:"description"`
	// Sponsor is the signed-in person who mints the identity's tokens and sees its runs.
	Sponsor string `json:"sponsor"`
	// UserType is an existing user type; it sets the identity's ceiling.
	UserType string `json:"user_type"`
}

// UpdateServiceIdentityRequest is the PATCH /api/v1/service-identities/{name} body: the fields present
// change. internal/api aliases it (updateServiceIdentityRequest).
type UpdateServiceIdentityRequest struct {
	Description *string `json:"description,omitempty"`
	Sponsor     *string `json:"sponsor,omitempty"`
	Suspended   *bool   `json:"suspended,omitempty"`
}

// MintServiceTokenRequest is the POST /api/v1/me/service-identities/{name}/tokens body. internal/api
// aliases it (serviceTokenRequest).
type MintServiceTokenRequest struct {
	Name string `json:"name"`
	// TTLSeconds is at most 90 days, or the deployment's cap if lower; zero takes that longest lifetime.
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// MoveTokenRequest is the POST /api/v1/tokens/{id}/move body. internal/api aliases it (moveTokenRequest).
type MoveTokenRequest struct {
	ServiceIdentity string `json:"service_identity"`
}

func serviceIdentityPath(name string) string {
	return "/api/v1/service-identities/" + url.PathEscape(name)
}

func myServiceTokensPath(name string) string {
	return "/api/v1/me/service-identities/" + url.PathEscape(name) + "/tokens"
}

// CreateServiceIdentity creates a named, sponsored non-human principal (security tier). With the
// second-human governance rule on, the create is held and the call returns a PendingApprovalError.
// POST /api/v1/service-identities.
func (c *Client) CreateServiceIdentity(ctx context.Context, req CreateServiceIdentityRequest) (ServiceIdentity, error) {
	var out ServiceIdentity
	err := c.do(ctx, http.MethodPost, "/api/v1/service-identities", req, &out)
	return out, err
}

// ListServiceIdentities lists every service identity with its derived state (security tier).
// GET /api/v1/service-identities.
func (c *Client) ListServiceIdentities(ctx context.Context, opts ...ListOpts) ([]ServiceIdentity, error) {
	out, _, err := c.ListServiceIdentitiesPage(ctx, opts...)
	return out, err
}

// ListServiceIdentitiesPage is ListServiceIdentities plus the server's X-Wardyn-Truncated signal.
func (c *Client) ListServiceIdentitiesPage(ctx context.Context, opts ...ListOpts) (identities []ServiceIdentity, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/service-identities", opts), nil, &identities, &hdr)
	return identities, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// GetServiceIdentity reads one service identity (security tier). GET /api/v1/service-identities/{name}.
func (c *Client) GetServiceIdentity(ctx context.Context, name string) (ServiceIdentity, error) {
	var out ServiceIdentity
	err := c.do(ctx, http.MethodGet, serviceIdentityPath(name), nil, &out)
	return out, err
}

// UpdateServiceIdentity changes an identity's description, sponsor or suspended state (security tier).
// A sponsor change revokes every token of the identity. PATCH /api/v1/service-identities/{name}.
func (c *Client) UpdateServiceIdentity(ctx context.Context, name string, req UpdateServiceIdentityRequest) (ServiceIdentity, error) {
	var out ServiceIdentity
	err := c.do(ctx, http.MethodPatch, serviceIdentityPath(name), req, &out)
	return out, err
}

// RetireServiceIdentity retires an identity for good, revoking its tokens (security tier); calling it
// again finishes an erase that failed. DELETE /api/v1/service-identities/{name}.
func (c *Client) RetireServiceIdentity(ctx context.Context, name string) (ServiceIdentity, error) {
	var out ServiceIdentity
	err := c.do(ctx, http.MethodDelete, serviceIdentityPath(name), nil, &out)
	return out, err
}

// ListMyServiceIdentities lists the identities the caller sponsors (a signed-in person).
// GET /api/v1/me/service-identities.
func (c *Client) ListMyServiceIdentities(ctx context.Context, opts ...ListOpts) ([]ServiceIdentity, error) {
	var out []ServiceIdentity
	err := c.do(ctx, http.MethodGet, appendListOpts("/api/v1/me/service-identities", opts), nil, &out)
	return out, err
}

// MintServiceToken mints a token of a service identity the caller sponsors; Token holds the plaintext,
// which the server shows once. It needs the sponsor's signed-in browser session, so a bearer token
// cannot mint: the CLI has no verb for it. POST /api/v1/me/service-identities/{name}/tokens.
func (c *Client) MintServiceToken(ctx context.Context, name string, req MintServiceTokenRequest) (APIToken, error) {
	var out APIToken
	err := c.do(ctx, http.MethodPost, myServiceTokensPath(name), req, &out)
	return out, err
}

// ListServiceTokens lists the tokens of an identity the caller sponsors; no row carries the plaintext.
// GET /api/v1/me/service-identities/{name}/tokens.
func (c *Client) ListServiceTokens(ctx context.Context, name string, opts ...ListOpts) ([]APIToken, error) {
	out, _, err := c.ListServiceTokensPage(ctx, name, opts...)
	return out, err
}

// ListServiceTokensPage is ListServiceTokens plus the server's X-Wardyn-Truncated signal.
func (c *Client) ListServiceTokensPage(ctx context.Context, name string, opts ...ListOpts) (tokens []APIToken, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts(myServiceTokensPath(name), opts), nil, &tokens, &hdr)
	return tokens, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// RevokeServiceToken revokes one token of an identity the caller sponsors.
// DELETE /api/v1/me/service-identities/{name}/tokens/{id}.
func (c *Client) RevokeServiceToken(ctx context.Context, name string, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, myServiceTokensPath(name)+"/"+id.String(), nil, nil)
}

// MoveToken links a live admin-minted token to the service identity that replaces it (security tier).
// It never revokes: the old token keeps working until an admin revokes it. POST /api/v1/tokens/{id}/move.
func (c *Client) MoveToken(ctx context.Context, id uuid.UUID, serviceIdentity string) (TokenMoved, error) {
	var out TokenMoved
	err := c.do(ctx, http.MethodPost, "/api/v1/tokens/"+id.String()+"/move", MoveTokenRequest{ServiceIdentity: serviceIdentity}, &out)
	return out, err
}
