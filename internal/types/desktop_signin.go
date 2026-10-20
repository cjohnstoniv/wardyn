// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DesktopSignin is a person signing in to websites inside a run's desktop
// (issue #2972, migration 0199). It holds no cookie and no credential: only
// the SHA-256 of the sign-in lane credential the proxy checks.
type DesktopSignin struct {
	ID             uuid.UUID       `json:"id"`
	RunID          uuid.UUID       `json:"run_id"`
	Holder         string          `json:"holder"`
	CredentialHash []byte          `json:"-"`
	PauseRecord    json.RawMessage `json:"-"`
	BeganAt        time.Time       `json:"began_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	EndRequested   string          `json:"-"`
	// EndStage is the step an end that could not finish is stuck at.
	EndStage  string     `json:"-"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

// DesktopSigninShare is one site's login a person shared with the run's agent.
type DesktopSigninShare struct {
	ID          uuid.UUID `json:"id"`
	SigninID    uuid.UUID `json:"signin_id"`
	RunID       uuid.UUID `json:"run_id"`
	Site        string    `json:"site"`
	CookieCount int       `json:"cookies"`
	SharedBy    string    `json:"shared_by"`
	SharedAt    time.Time `json:"shared_at"`
}
