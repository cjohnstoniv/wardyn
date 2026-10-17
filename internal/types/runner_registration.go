// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// ClaimExpired also rejects an invalid zero creation time. Claimed identities
// do not expire under this rule; revocation is a separate terminal state.
func (r Runner) ClaimExpired(now time.Time) bool {
	return r.State == RunnerUnclaimed && !r.CreatedAt.Add(RunnerUnclaimedTTL).After(now)
}

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
	// OrgURL is the organisation URL the runner must register against: the mint answers carry it, a
	// token list never does. wardyn-runnerd refuses an --org that does not hash to the server's.
	OrgURL string `json:"org_url,omitempty"`
	// ProxyImage is the egress-proxy image this control plane runs its own sandboxes with. The mint answers
	// carry it so a runner set up from the token can start the same one; omitted when none is configured.
	ProxyImage string `json:"proxy_image,omitempty"`
}
