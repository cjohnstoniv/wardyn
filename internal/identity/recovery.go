// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"time"
)

// RecoveryVerifier is the optional half of a Provider that lets a run token which expired while the control plane was
// unreachable be presented once more, to the recovery door only. A Provider without it recovers nothing.
type RecoveryVerifier interface {
	// VerifyExpired authenticates token as Verify does, except that it requires the token to have EXPIRED: signature and
	// audience valid, and now at or after its expiry. An unexpired token is refused with ErrNotExpired, so the caller
	// sends it to renew. A revoked token is refused with *RevokedTokenError. A revocation store that cannot be read is
	// an ordinary error, not a revocation: the door answers it as retryable, and no token is issued either way.
	// A token that expired more than oldest ago is refused with ErrTokenTooOld before the revocation store is read,
	// so a holder of an old token costs nothing past the signature check.
	VerifyExpired(ctx context.Context, token, expectedAudience string, oldest time.Duration) (*Claims, error)
}

// ErrNotExpired is VerifyExpired's refusal of a token that is still valid.
var ErrNotExpired = errors.New("identity: the token has not expired")

// ErrTokenTooOld is VerifyExpired's refusal of a token that expired longer ago than the caller recovers.
var ErrTokenTooOld = errors.New("identity: the token expired too long ago to recover")
