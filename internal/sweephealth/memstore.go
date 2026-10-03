// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sweephealth

import (
	"context"
	"maps"
	"sync"
	"time"
)

// MemStore is an in-process Store with the same never-backwards rule as the
// Postgres one. Replicas in a test share one MemStore the way replicas in
// production share the sweep_ticks table.
type MemStore struct {
	mu    sync.Mutex
	ticks map[string]Tick
	err   error
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{ticks: map[string]Tick{}} }

// SetErr makes every later call fail with err (nil restores it): a store that
// is down.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

// RecordTick implements Store.
func (m *MemStore) RecordTick(_ context.Context, sweep, replica string, success bool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	t := m.ticks[sweep]
	if !at.Before(t.AttemptedAt) {
		t.Replica = replica
	}
	if at.After(t.AttemptedAt) {
		t.AttemptedAt = at
	}
	if success && at.After(t.SucceededAt) {
		t.SucceededAt = at
	}
	m.ticks[sweep] = t
	return nil
}

// Ticks implements Store.
func (m *MemStore) Ticks(context.Context) (map[string]Tick, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return maps.Clone(m.ticks), nil
}
