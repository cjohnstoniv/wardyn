// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestStatePagesExceedsLifetimeRowLimitWithoutAggregation(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	m.Offline(t.Context())
	m.mu.Lock()
	for range 1001 {
		id := uuid.New()
		m.state.Runs[id] = localRun{RunID: id, Ref: id.String(), Lease: runnerwire.Lease{RunID: id}}
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	cursor := runnerwire.StateInventory{}
	pages, count := 0, 0
	if err := m.StatePages(t.Context(), func(page runnerwire.StatePage) error {
		next, err := cursor.Next(page)
		if err != nil {
			return err
		}
		pages++
		count += len(page.Runs)
		cursor = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1001 || pages != 11 || !cursor.Complete {
		t.Fatalf("inventory count=%d pages=%d complete=%v", count, pages, cursor.Complete)
	}
}

func TestStatePagesInterruptedInventoryNeverEmitsFinal(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	m.Offline(t.Context())
	for range runnerwire.StatePageRows + 1 {
		id := uuid.New()
		m.state.Runs[id] = localRun{RunID: id, Ref: id.String(), Lease: runnerwire.Lease{RunID: id}}
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("durable intake unavailable")
	calls := 0
	var oldID uuid.UUID
	if err := m.StatePages(t.Context(), func(page runnerwire.StatePage) error {
		calls++
		oldID = page.InventoryID
		if page.Final {
			t.Fatal("incomplete page marked final")
		}
		return stopped
	}); !errors.Is(err, stopped) || calls != 1 {
		t.Fatalf("interruption ignored: calls=%d err=%v", calls, err)
	}
	if err := m.StatePages(t.Context(), func(page runnerwire.StatePage) error {
		if page.InventoryID == oldID {
			t.Fatal("new inventory reused interrupted snapshot identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStatePagesRequireClosedAdmissionAndDurableSnapshot(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	calls := 0
	send := func(runnerwire.StatePage) error { calls++; return nil }
	if err := m.StatePages(t.Context(), send); err == nil || calls != 0 {
		t.Fatal("online execution admitted during inventory")
	}
	m.Offline(t.Context())
	restore := failStateSave(t, m)
	m.dirty = true
	if err := m.StatePages(t.Context(), send); err == nil || calls != 0 {
		t.Fatal("unpersisted snapshot published")
	}
	restore()
	if err := m.StatePages(t.Context(), send); err != nil || calls != 1 {
		t.Fatalf("empty final: calls=%d %v", calls, err)
	}
}
