// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file is the CONCURRENT companion to revoke_order_test.go (U017): those
// tests pin the C002/C003 semantics for a CAS whose outcome is already decided;
// these actually race a kill against a dispatch forward-transition on one run
// under `go test -race`, asserting exactly one writer wins the state cell and
// the loser performs no revocation. It is the safety net required before any
// structural change to runs.go's dispatch/kill code.

// raceStore is a minimal race-safe Store: one run, one genuinely atomic
// compare-and-set state cell.
type raceStore struct {
	store.Store
	mu    sync.Mutex
	run   types.AgentRun
	state types.RunState
}

func (s *raceStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run
	r.State = s.state
	return r, nil
}

func (s *raceStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *raceStore) State() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// raceBroker records revocations race-safely.
type raceBroker struct {
	fakeBroker
	mu sync.Mutex
	n  map[uuid.UUID]int
}

func (b *raceBroker) RevokeRun(_ context.Context, runID uuid.UUID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == nil {
		b.n = map[uuid.UUID]int{}
	}
	b.n[runID]++
	return nil
}

func (b *raceBroker) revocations(runID uuid.UUID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n[runID]
}

// raceAudit is a race-safe no-op audit recorder (the harness recRecorder
// appends without a lock and would itself trip the race detector).
type raceAudit struct{ mu sync.Mutex }

func (r *raceAudit) Record(context.Context, types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil
}

// TestKillRun_DispatchForwardRace_ExactlyOneWinner races handleKillRun against
// the dispatch forward-transition (PENDING→STARTING) on the same run, repeatedly.
// Legal serializations: kill owns PENDING first (forward loses), forward moves
// first and the kill then legitimately kills the STARTING run (both "win"), or
// forward moves between the kill's read and its CAS (kill 409s). The invariants,
// every iteration:
//   - kill reported a win (202/500) → final state KILLED, ≥1 revocation;
//   - kill reported 409 → forward won, final state STARTING, ZERO revocations
//     (C002: a losing kill must never strip a still-live run's credentials).
//
// Run under -race this also proves the handler path itself is data-race-free.
func TestKillRun_DispatchForwardRace_ExactlyOneWinner(t *testing.T) {
	h := newHarness(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		runID := uuid.New()
		st := &raceStore{
			run:   types.AgentRun{ID: runID},
			state: types.RunPending,
		}
		brk := &raceBroker{}
		cfg := baseTestConfig(h, st)
		cfg.Broker = brk
		cfg.Audit = &raceAudit{}
		srv := New(cfg)

		start := make(chan struct{})
		var (
			wg         sync.WaitGroup
			killCode   int
			forwardWon bool
		)
		wg.Add(2)
		go func() { // the kill switch
			defer wg.Done()
			<-start
			w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
			killCode = w.Code
		}()
		go func() { // dispatch's forward-transition, as dispatch performs it
			defer wg.Done()
			<-start
			forwardWon, _ = st.UpdateRunStateIf(context.Background(), runID, types.RunPending, types.RunStarting)
		}()
		close(start)
		wg.Wait()

		// 202 = clean kill; 500 = honest partial-teardown report — both OWN the
		// KILLED transition. Only 409 is a loss.
		killWon := killCode == http.StatusAccepted || killCode == http.StatusInternalServerError
		revs := brk.revocations(runID)
		final := st.State()

		if killWon {
			if final != types.RunKilled {
				t.Fatalf("round %d: kill won but final state=%s", i, final)
			}
			if revs < 1 {
				t.Fatalf("round %d: kill won the CAS but never revoked", i)
			}
		} else {
			if killCode != http.StatusConflict {
				t.Fatalf("round %d: losing kill must 409, got %d", i, killCode)
			}
			if !forwardWon {
				t.Fatalf("round %d: kill 409'd but forward also lost — nobody moved the run; final=%s", i, final)
			}
			if revs != 0 {
				t.Fatalf("round %d: losing kill revoked a still-live run's credentials (C002); revocations=%d", i, revs)
			}
			if final != types.RunStarting {
				t.Fatalf("round %d: forward won but final state=%s", i, final)
			}
		}
	}
}

// TestKillRun_FailAndRevokeRace_SingleRevocation races handleKillRun against
// failAndRevoke (a create/dispatch failure path, C003) — two TERMINAL writers.
// Exactly one may win, and the run's credentials are revoked exactly once.
func TestKillRun_FailAndRevokeRace_SingleRevocation(t *testing.T) {
	h := newHarness(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		runID := uuid.New()
		st := &raceStore{
			run:   types.AgentRun{ID: runID},
			state: types.RunPending,
		}
		brk := &raceBroker{}
		cfg := baseTestConfig(h, st)
		cfg.Broker = brk
		cfg.Audit = &raceAudit{}
		srv := New(cfg)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			srv.failAndRevoke(context.Background(), runID, types.RunPending)
		}()
		close(start)
		wg.Wait()

		final := st.State()
		if final != types.RunKilled && final != types.RunFailed {
			t.Fatalf("round %d: final state must be a terminal winner's, got %s", i, final)
		}
		if revs := brk.revocations(runID); revs != 1 {
			t.Fatalf("round %d: two racing terminal writers must revoke exactly once, got %d (final=%s)", i, revs, final)
		}
	}
}
