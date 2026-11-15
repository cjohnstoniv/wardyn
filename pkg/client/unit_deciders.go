// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DeciderRule is one unit deciders rule: the hold kinds it governs and who may decide them.
type DeciderRule = types.DeciderRule

// UnitDeciders is the stored deciders rules of one unit.
type UnitDeciders = types.UnitDeciders

// PutUnitDeciders replaces the deciders rules of the unit unitID; an empty list clears them. PUT
// /api/v1/governance/units/{id}/deciders. A write the server holds for a second approver (always, when
// the caller is a unit admin) comes back as the pending change and zero rules.
func (c *Client) PutUnitDeciders(ctx context.Context, unitID uuid.UUID, rules []DeciderRule) (UnitDeciders, *GovernanceChange, error) {
	return c.putUnitDeciders(ctx, unitID, rules, "")
}

// putUnitDeciders is PutUnitDeciders with query (a prune's "?under=<unit id>", or "") on the request.
func (c *Client) putUnitDeciders(ctx context.Context, unitID uuid.UUID, rules []DeciderRule, query string) (UnitDeciders, *GovernanceChange, error) {
	var out UnitDeciders
	if rules == nil {
		rules = []DeciderRule{}
	}
	body := struct {
		Rules []DeciderRule `json:"rules"`
	}{Rules: rules}
	pending, err := c.doPending(ctx, http.MethodPut, "/api/v1/governance/units/"+unitID.String()+"/deciders"+query, body, &out)
	return out, pending, err
}
