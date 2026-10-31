// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GetMyWalls reads the caller's own walls (GET /api/v1/me/walls): an empty spec when none are set.
func (c *Client) GetMyWalls(ctx context.Context) (PersonalWalls, error) {
	var out PersonalWalls
	err := c.do(ctx, http.MethodGet, "/api/v1/me/walls", nil, &out)
	return out, err
}

// PutMyWalls replaces the caller's walls with spec (PUT /api/v1/me/walls). The walls only narrow:
// the server refuses a spec that names anything wider, and an empty spec is no walls.
func (c *Client) PutMyWalls(ctx context.Context, spec PersonalWallsSpec) (PersonalWalls, error) {
	var out PersonalWalls
	err := c.do(ctx, http.MethodPut, "/api/v1/me/walls", types.PersonalWalls{Spec: spec}, &out)
	return out, err
}

// ClearMyWalls removes the caller's walls (DELETE /api/v1/me/walls).
func (c *Client) ClearMyWalls(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/walls", nil, nil)
}
