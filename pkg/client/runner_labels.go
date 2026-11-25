// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerLabelsRequest is the body of SetRunnerLabels: a runner's whole operator label set.
type RunnerLabelsRequest = types.RunnerLabelsRequest

// SetRunnerLabels replaces a claimed runner's operator labels (a security operator); an empty map clears them.
func (c *Client) SetRunnerLabels(ctx context.Context, id uuid.UUID, labels map[string]string) (RunnerView, error) {
	var out RunnerView
	if labels == nil {
		labels = map[string]string{}
	}
	err := c.do(ctx, http.MethodPut, "/api/v1/runners/"+id.String()+"/labels", types.RunnerLabelsRequest{Labels: labels}, &out)
	return out, err
}
