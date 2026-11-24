// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// DrainRunner drains a registered runner (an administrator): it takes no new sandbox, and its running runs go on.
func (c *Client) DrainRunner(ctx context.Context, id uuid.UUID, reason string) (RunnerView, error) {
	var out RunnerView
	err := c.do(ctx, http.MethodPut, "/api/v1/runners/"+id.String()+"/drain", RunnerDrainRequest{Reason: reason}, &out)
	return out, err
}

// UndrainRunner undrains a registered runner (an administrator), so placement may choose it again.
func (c *Client) UndrainRunner(ctx context.Context, id uuid.UUID) (RunnerView, error) {
	var out RunnerView
	err := c.do(ctx, http.MethodDelete, "/api/v1/runners/"+id.String()+"/drain", nil, &out)
	return out, err
}
