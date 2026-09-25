// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PolicyRequest is the body for POST/PUT /api/v1/policies. Name is required;
// Spec is validated server-side before persistence (a bad spec is rejected
// with 400, fail closed).
type PolicyRequest struct {
	Name string              `json:"name"`
	Spec types.RunPolicySpec `json:"spec"`
}

// ListPoliciesPage is ListPolicies plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists.
func (c *Client) ListPoliciesPage(ctx context.Context, opts ...ListOpts) (policies []types.RunPolicy, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/policies", opts), nil, &policies, &hdr)
	return policies, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListPolicies returns run policies in reverse creation order. Pass a ListOpts to page.
// Prefer ListPoliciesPage, which also returns the server's truncation signal.
func (c *Client) ListPolicies(ctx context.Context, opts ...ListOpts) ([]types.RunPolicy, error) {
	policies, _, err := c.ListPoliciesPage(ctx, opts...)
	return policies, err
}

// GetPolicy fetches a single RunPolicy by its UUID.
// Returns 404/APIError when the policy does not exist.
func (c *Client) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodGet, "/api/v1/policies/"+id.String(), nil, &out)
	return out, err
}

// GetDefaultPolicy fetches the control plane's configured default policy
// spec — the ceiling every run created without a policy_id gets, and the
// ceiling composer.Clamp bounds a member-authored inline policy against
// (W14-S1-6: previously unexposed by UI, CLI or API).
func (c *Client) GetDefaultPolicy(ctx context.Context) (types.RunPolicySpec, error) {
	var out types.RunPolicySpec
	err := c.do(ctx, http.MethodGet, "/api/v1/policies/default", nil, &out)
	return out, err
}

// CreatePolicy validates and persists a new policy.
// Returns the created RunPolicy (status 201) on success; 400 on an invalid
// name or spec.
func (c *Client) CreatePolicy(ctx context.Context, req PolicyRequest) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPost, "/api/v1/policies", req, &out)
	return out, err
}

// UpdatePolicy validates and replaces an existing policy's name and spec.
// Returns the updated RunPolicy on success; 404 when unknown; 400 when invalid.
func (c *Client) UpdatePolicy(ctx context.Context, id uuid.UUID, req PolicyRequest) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPut, "/api/v1/policies/"+id.String(), req, &out)
	return out, err
}

// DeletePolicy removes a policy by id.
// Returns nil on success (204); 404/APIError when the policy does not exist.
func (c *Client) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/policies/"+id.String(), nil, nil)
}
