// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// GetRunWallPreset reads the wall preset a run was launched with from GET /api/v1/runs/{id}: nil for a
// run launched with none, and never the preset's spec.
func (c *Client) GetRunWallPreset(ctx context.Context, id uuid.UUID) (*RunWallPreset, error) {
	var out struct {
		WallPreset *RunWallPreset `json:"wall_preset"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+id.String(), nil, &out); err != nil {
		return nil, err
	}
	return out.WallPreset, nil
}
