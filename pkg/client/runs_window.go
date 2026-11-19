// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// MacWindowGrant is the answer of POST /runs/{id}/desktop/window: the window is up on the Mac.
type MacWindowGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	RunnerID uuid.UUID `json:"runner_id"`
	// Runner names the computer the window is on.
	Runner  string `json:"runner,omitempty"`
	Trigger string `json:"trigger"`
}

// MacWindowLastClose is why the caller's last window on a run closed.
type MacWindowLastClose struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// MacWindowView is GET /runs/{id}'s mac_window: whether the caller can open a window now. State "open" means
// they can; any other state carries the server's sentence in Message, except for a caller who isn't the
// run's owner.
type MacWindowView struct {
	State     string              `json:"state"`
	Message   string              `json:"message,omitempty"`
	LastClose *MacWindowLastClose `json:"last_close,omitempty"`
}

// OpenMacWindow asks for an attended window onto a macOS VM run on the caller's own Mac. It returns once the
// window is up; a refusal is an *APIError carrying the server's sentence.
func (c *Client) OpenMacWindow(ctx context.Context, runID uuid.UUID) (MacWindowGrant, error) {
	var out MacWindowGrant
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+runID.String()+"/desktop/window", map[string]string{"trigger": "cli"}, &out)
	return out, err
}

// GetMacWindow reads the run's mac_window; nil when the run has none (not a live macOS VM run).
func (c *Client) GetMacWindow(ctx context.Context, runID uuid.UUID) (*MacWindowView, error) {
	var out struct {
		MacWindow *MacWindowView `json:"mac_window"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String(), nil, &out)
	return out.MacWindow, err
}
