// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// TemplatePolicyCheckRequest is the body of POST /api/v1/templates/{id}/policy-check: the candidate
// policy to measure against the template. A body with no candidate is refused, never read as an
// empty policy.
type TemplatePolicyCheckRequest struct {
	Candidate *RunPolicySpec `json:"candidate"`
}

// PolicyWidening is one policy field that keeps the candidate from being within: the field's JSON
// name and, for a list field, the entries that are not within.
type PolicyWidening struct {
	Field   string   `json:"field"`
	Entries []string `json:"entries,omitempty"`
}

// PolicyCheckResult is the server's verdict. Within is false unless the server said true, so an
// answer that lost the field reads as not within. Widenings is empty when Within is true.
type PolicyCheckResult struct {
	Within    bool             `json:"within"`
	Widenings []PolicyWidening `json:"widenings"`
}

// CheckTemplatePolicy asks whether candidate is within the template's policy as it resolves for
// the caller: the server lays the template's policy keys onto the policy the template takes for
// them, as a launch would, and judges with the rule `wardyn policy check` uses. The answer never
// carries the resolved policy. A template the caller cannot read is the 404 a missing one gets.
func (c *Client) CheckTemplatePolicy(ctx context.Context, id uuid.UUID, candidate RunPolicySpec) (PolicyCheckResult, error) {
	var out PolicyCheckResult
	err := c.do(ctx, http.MethodPost, "/api/v1/templates/"+id.String()+"/policy-check", TemplatePolicyCheckRequest{Candidate: &candidate}, &out)
	return out, err
}
