// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// LocalWindowGrant is the answer of POST /runs/{id}/desktop/local-window: the organisation granted the
// window and the runner is showing the run's display on its local socket.
type LocalWindowGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	RunnerID uuid.UUID `json:"runner_id"`
	// Runner names the computer the window is on.
	Runner string `json:"runner,omitempty"`
}

// OpenLocalWindow asks the organisation to grant a window onto a Linux VM run for localNonce, the nonce
// the runner's local socket on the caller's own computer handed out for a window. A refusal is an
// *APIError carrying the server's sentence.
func (c *Client) OpenLocalWindow(ctx context.Context, runID uuid.UUID, localNonce string) (LocalWindowGrant, error) {
	var out LocalWindowGrant
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+runID.String()+"/desktop/local-window", map[string]string{"local_nonce": localNonce}, &out)
	return out, err
}
