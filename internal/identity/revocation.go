// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"

	"github.com/google/uuid"
)

// RevocationStore backs the kill-switch denylist for run identities — the
// generic contract an identity provider's revocation backend satisfies. The
// pg-backed implementation lives in the control plane (cmd/wardynd); the embedded
// provider ships an in-memory one for tests.
//
// All read methods MUST fail closed at the call site: a provider treats any
// IsRevoked error as a revoked token. Revocation is jti-level OR run-level;
// RevokeRun is the kill-switch cascade (invalidates every current and future
// token for a run without enumerating jtis).
type RevocationStore interface {
	IsRevoked(ctx context.Context, jti string, runID uuid.UUID) (bool, error)
	RevokeRun(ctx context.Context, runID uuid.UUID) error
	RevokeJTI(ctx context.Context, jti string, runID uuid.UUID) error
}
