// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
)

// TestRetirementCallsTheRetiredHook: a retirement that succeeds calls OnRetired's hook with the run,
// once, with mu released; a deferred one does not. The retired run then leaves EvidenceRunIDs, so
// startup never replays its relay key.
func TestRetirementCallsTheRetiredHook(t *testing.T) {
	m, _, id, spool, a := retirementFixture(t)
	m.mu.Lock()
	r := m.state.Runs[id]
	r.Substrate = json.RawMessage(`{"relay_key_hash":"00"}`)
	m.state.Runs[id] = r
	m.mu.Unlock()
	if !slices.Contains(m.EvidenceRunIDs(), id) {
		t.Fatal("setup: the run is not among EvidenceRunIDs")
	}
	var got []uuid.UUID
	m.OnRetired(func(run uuid.UUID) {
		if !m.mu.TryLock() {
			t.Error("the hook ran under mu")
		} else {
			m.mu.Unlock()
		}
		got = append(got, run)
	})
	if err := spool.Append(t.Context(), egress.DecisionLog{Request: egress.Request{RunID: id}}); err != nil {
		t.Fatal(err)
	}
	if res, err := m.apply(t.Context(), a); err != nil || res.Outcome != "deferred" || len(got) != 0 {
		t.Fatalf("deferred retirement: %+v %v, hook calls %v", res, err, got)
	}
	if err := spool.Replay(t.Context(), func(context.Context, []runnerspool.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	a.ID = uuid.New()
	proof := *a.Retirement
	a.Retirement = &proof
	receipt, err := spool.Receipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a.Retirement.Spool = receipt
	if res, err := m.apply(t.Context(), a); err != nil || res.Outcome != "success" {
		t.Fatalf("retire: %+v %v", res, err)
	}
	if len(got) != 1 || got[0] != id {
		t.Fatalf("hook calls %v, want [%s]", got, id)
	}
	if slices.Contains(m.EvidenceRunIDs(), id) {
		t.Fatal("a retired run is still among EvidenceRunIDs, so startup would replay its relay key")
	}
}
