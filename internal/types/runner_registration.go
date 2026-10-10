// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// RunnerRegistrationToken is a single-use invitation for one owner's runner.
// The raw token is returned only at mint; stored rows carry its hash.
type RunnerRegistrationToken struct {
	ID           uuid.UUID  `json:"id"`
	Owner        string     `json:"owner"`
	MintedBy     string     `json:"minted_by"`
	Token        string     `json:"token,omitempty"`
	TokenSHA256  string     `json:"-"`
	OrgURLSHA256 string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	ConsumedAt   *time.Time `json:"consumed_at,omitempty"`
}
