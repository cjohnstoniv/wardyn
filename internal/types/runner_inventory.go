// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// RunnerUnclaimedTTL is how long an unclaimed runner waits for its owner.
const RunnerUnclaimedTTL = 24 * time.Hour

// RunnerView is a runner as the management routes show it to a person or an administrator. It is
// never sent to a runner. Every posture fact is what the runner said about itself.
type RunnerView struct {
	ID    uuid.UUID   `json:"id"`
	Name  string      `json:"name"`
	State RunnerState `json:"state"`
	// Owner is set on the administrators' routes only; the caller's own list omits it.
	Owner string `json:"owner,omitempty"`
	// MintedBy names who minted the registration token this runner redeemed.
	MintedBy string `json:"minted_by,omitempty"`
	// Online is the hub's live view of a claimed runner and false for any other.
	Online bool `json:"online"`
	// KeyFingerprint is abbreviated for an unclaimed runner on its owner's own routes, so it cannot
	// be copied from there into the claim; KeyFingerprintAbbreviated says so.
	KeyFingerprint            string    `json:"key_fingerprint"`
	KeyFingerprintAbbreviated bool      `json:"key_fingerprint_abbreviated"`
	Version                   string    `json:"version,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	// ClaimExpiresAt is when an unclaimed runner lapses; absent for any other state.
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	ClaimedAt      *time.Time `json:"claimed_at,omitempty"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	// Posture is absent until the runner has reported; PostureSource is then runner_asserted.
	Posture           *RunnerPosture `json:"posture,omitempty"`
	PostureReportedAt *time.Time     `json:"posture_reported_at,omitempty"`
	PostureSource     string         `json:"posture_source,omitempty"`
	// RunsActive counts the non-terminal runs on this runner.
	RunsActive int `json:"runs_active"`
}

// RunnerSettingsRequest turns runners on or off. Enabled is required, so an empty body never
// switches anything.
type RunnerSettingsRequest struct {
	Enabled *bool `json:"enabled"`
}
