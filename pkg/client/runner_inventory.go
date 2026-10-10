// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerView is a registered runner as the management routes show it. Posture and
// everything else a runner says about itself is as reported, never verified.
type RunnerView = types.RunnerView

// RunnerSettings is the runners on/off switch.
type RunnerSettings = types.RunnerSettings

// The runner state filter of ListRunners; an empty filter is RunnerFilterActive.
const (
	RunnerFilterActive  = "active"
	RunnerFilterRevoked = "revoked"
	RunnerFilterAll     = "all"
)

// ListRunners lists every person's runners (admin or security_admin). state is
// RunnerFilterActive, RunnerFilterRevoked or RunnerFilterAll; empty is active.
func (c *Client) ListRunners(ctx context.Context, state string) ([]RunnerView, error) {
	path := "/api/v1/runners"
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	var out []RunnerView
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetRunner reads one runner, revoked included (admin or security_admin).
func (c *Client) GetRunner(ctx context.Context, id uuid.UUID) (RunnerView, error) {
	var out RunnerView
	err := c.do(ctx, http.MethodGet, "/api/v1/runners/"+id.String(), nil, &out)
	return out, err
}

// ListMyRunners lists the caller's own runners; an unclaimed one's fingerprint is abbreviated.
func (c *Client) ListMyRunners(ctx context.Context) ([]RunnerView, error) {
	var out []RunnerView
	err := c.do(ctx, http.MethodGet, "/api/v1/me/runners", nil, &out)
	return out, err
}

// ListRunnerTokens lists the unused registration tokens (admin or security_admin). No token value is returned.
func (c *Client) ListRunnerTokens(ctx context.Context) ([]RunnerRegistrationToken, error) {
	var out []RunnerRegistrationToken
	err := c.do(ctx, http.MethodGet, "/api/v1/runners/tokens", nil, &out)
	return out, err
}

// RevokeRunnerToken stops an unused registration token being redeemed (admin or security_admin).
func (c *Client) RevokeRunnerToken(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/runners/tokens/"+id.String(), nil, nil)
}

// GetRunnerSettings reads whether runners are on (admin or security_admin).
func (c *Client) GetRunnerSettings(ctx context.Context) (RunnerSettings, error) {
	var out RunnerSettings
	err := c.do(ctx, http.MethodGet, "/api/v1/runners/settings", nil, &out)
	return out, err
}

// SetRunnersEnabled turns runners on or off (admin). Turning on is refused until the public
// organisation URL is HTTPS.
func (c *Client) SetRunnersEnabled(ctx context.Context, enabled bool) (RunnerSettings, error) {
	var out RunnerSettings
	err := c.do(ctx, http.MethodPut, "/api/v1/runners/settings", types.RunnerSettingsRequest{Enabled: &enabled}, &out)
	return out, err
}
