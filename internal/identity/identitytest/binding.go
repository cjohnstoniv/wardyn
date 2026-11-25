// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// MemBindingStore is an in-memory identity.TokenBindingStore for tests. SetErr makes every call fail,
// for the fail-closed cases.
type MemBindingStore struct {
	mu   sync.Mutex
	rows map[string]identity.TokenBinding
	err  error
}

// NewMemBindingStore returns an empty store.
func NewMemBindingStore() *MemBindingStore {
	return &MemBindingStore{rows: make(map[string]identity.TokenBinding)}
}

// SetErr makes every later call return err; nil restores the store.
func (m *MemBindingStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Len is the number of rows held.
func (m *MemBindingStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func (m *MemBindingStore) PutBinding(_ context.Context, b identity.TokenBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if old, ok := m.rows[b.JTI]; ok {
		if identity.SameBinding(old, b) {
			return nil
		}
		return fmt.Errorf("identitytest: a different binding for jti %q already exists", b.JTI)
	}
	m.rows[b.JTI] = b
	return nil
}

func (m *MemBindingStore) GetBinding(_ context.Context, jti string) (identity.TokenBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return identity.TokenBinding{}, m.err
	}
	b, ok := m.rows[jti]
	if !ok {
		return identity.TokenBinding{}, identity.ErrBindingNotFound
	}
	return b, nil
}

func (m *MemBindingStore) PruneBindings(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	var n int64
	for jti, b := range m.rows {
		if n == identity.PruneBatch {
			break
		}
		if b.Expiry.Before(cutoff) {
			delete(m.rows, jti)
			n++
		}
	}
	return n, nil
}
