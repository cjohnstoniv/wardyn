// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// MemRevocationStore is an in-memory identity.RevocationStore for tests.
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
