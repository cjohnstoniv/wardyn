// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GovernanceEnvelope is a platform's root unit and the IdP groups the security tier approved for it.
// GET /api/v1/governance lists them as envelopes.
type GovernanceEnvelope = types.GovernanceEnvelope

type envelopeBody struct {
	ApprovedGroups []string `json:"approved_groups"`
}

// SetEnvelope makes groups the approved groups of the envelope on unitID, replacing any. PUT
// /api/v1/governance/units/{id}/envelope, security tier only. A set the server holds for a second
// human (WARDYN_GOVERNANCE_SECOND_HUMAN) comes back as the pending change and a zero envelope.
func (c *Client) SetEnvelope(ctx context.Context, unitID uuid.UUID, groups []string) (GovernanceEnvelope, *GovernanceChange, error) {
	var out GovernanceEnvelope
	if groups == nil {
		groups = []string{}
	}
	pending, err := c.doPending(ctx, http.MethodPut, "/api/v1/governance/units/"+unitID.String()+"/envelope", envelopeBody{ApprovedGroups: groups}, &out)
	return out, pending, err
}

// ClearEnvelope removes the envelope on unitID. DELETE /api/v1/governance/units/{id}/envelope. A clear
// the server holds for a second human comes back as the pending change.
func (c *Client) ClearEnvelope(ctx context.Context, unitID uuid.UUID) (*GovernanceChange, error) {
	return c.doPending(ctx, http.MethodDelete, "/api/v1/governance/units/"+unitID.String()+"/envelope", nil, nil)
}
