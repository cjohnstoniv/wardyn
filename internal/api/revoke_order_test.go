// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killCASStore serves a run to handleKillRun / failAndRevoke and controls whether
// the terminal CAS "wins".
type killCASStore struct {
	store.Store
	run        types.AgentRun
	casApplied bool
}

func (s *killCASStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) { return s.run, nil }
func (s *killCASStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return s.casApplied, nil
}

func revoked(list []uuid.UUID, id uuid.UUID) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// TestKillRun_LosesCASDoesNotRevoke is the C002 regression: the kill cascade used
// to revoke identity + broker credentials BEFORE its terminal CAS, so a kill that
// then LOST the CAS to a concurrent dispatch forward-transition had already stripped
// the credentials of a run that stays live — a zombie behind a silent 409. The CAS
// now runs first; on a loss the kill 409s WITHOUT revoking.
func TestKillRun_LosesCASDoesNotRevoke(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	fake := &killCASStore{
		run:        types.AgentRun{ID: runID, State: types.RunPending}, // non-terminal, no sandbox
		casApplied: false,                                              // a concurrent dispatch forward-transition won the row
	}
	cfg := baseTestConfig(h, fake)
	cfg.Broker = h.broker
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("a kill that loses the terminal CAS must 409; got %d: %s", w.Code, w.Body.String())
	}
	if revoked(h.broker.revoked, runID) {
		t.Fatalf("a kill that LOST the CAS must NOT revoke the still-live run's credentials (C002); broker.revoked=%v", h.broker.revoked)
	}
}

// TestFailAndRevoke_RevokesOnlyWhenTransitionWins is the C003 regression: every
// create/dispatch FAILED transition must revoke the run's minted credentials (not
// just flip state) — but only when THIS transition actually won, so a concurrent
// kill that already moved the run is not double-handled.
func TestFailAndRevoke_RevokesOnlyWhenTransitionWins(t *testing.T) {
	h := newHarness(t)
	fake := &killCASStore{casApplied: true}
	cfg := baseTestConfig(h, fake)
	cfg.Broker = h.broker
	srv := New(cfg)

	won := uuid.New()
	srv.failAndRevoke(context.Background(), won, types.RunStarting)
	if !revoked(h.broker.revoked, won) {
		t.Fatalf("a won FAILED transition must run the revoke cascade (C003); broker.revoked=%v", h.broker.revoked)
	}

	fake.casApplied = false
	h.broker.revoked = nil
	lost := uuid.New()
	srv.failAndRevoke(context.Background(), lost, types.RunStarting)
	if len(h.broker.revoked) != 0 {
		t.Errorf("a lost FAILED transition must NOT revoke (a concurrent kill owns it); broker.revoked=%v", h.broker.revoked)
	}
}
