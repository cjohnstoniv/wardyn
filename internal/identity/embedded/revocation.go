// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// RevocationStore is the embedded provider's revocation contract. It aliases
// identity.RevocationStore (the generic kill-switch denylist) so the identity
// seam's Deps can carry it without importing this package. The pg-backed
// implementation lives in the control plane; MemRevocationStore (below) is the
// in-memory one for tests. All reads fail closed at the call site (Verify treats
// any IsRevoked error as revoked).
type RevocationStore = identity.RevocationStore

// MemRevocationStore is an in-memory RevocationStore for tests.
type MemRevocationStore struct {
	mu         sync.RWMutex
	revokedJTI map[string]struct{}
	revokedRun map[uuid.UUID]struct{}
}

func NewMemRevocationStore() *MemRevocationStore {
	return &MemRevocationStore{
		revokedJTI: make(map[string]struct{}),
		revokedRun: make(map[uuid.UUID]struct{}),
	}
}

func (m *MemRevocationStore) IsRevoked(_ context.Context, jti string, runID uuid.UUID) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.revokedJTI[jti]; ok {
		return true, nil
	}
	_, ok := m.revokedRun[runID]
	return ok, nil
}

func (m *MemRevocationStore) RevokeRun(_ context.Context, runID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedRun[runID] = struct{}{}
	return nil
}

func (m *MemRevocationStore) RevokeJTI(_ context.Context, jti string, runID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedJTI[jti] = struct{}{}
	return nil
}
