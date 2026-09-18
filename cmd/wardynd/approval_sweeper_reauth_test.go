// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE SWEEPER IS WHERE A credential_reauth REQUEST ACTUALLY EXPIRES, and this
// drives that loop rather than the counter behind it (round-2 F2).
//
// The first shape called RecordCredentialReauthExpired directly, so deleting
// the three lines in runApprovalSweeper that call it left the pin green. By
// then the sidecar that held for the row gave up hours ago and no resolve will
// ever meet it, so if this loop does not count the expiry, nothing does.

// sweepStore is the smallest approval.Store the sweep uses: list PENDING, move
// a row, record the event.
type sweepStore struct {
	mu   sync.Mutex
	rows []types.ApprovalRequest
	recs []types.AuditEvent
}

func (s *sweepStore) CreateApproval(_ context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	return a, nil
}

func (s *sweepStore) GetApproval(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return types.ApprovalRequest{}, types.ErrDuplicatePendingApproval
}

func (s *sweepStore) ListApprovals(_ context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.ApprovalRequest{}
	for _, r := range s.rows {
		if state == "" || r.State == state {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *sweepStore) DecideApproval(_ context.Context, id uuid.UUID, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].State = d.State
			s.rows[i].DecidedBy = d.DecidedBy
			s.rows[i].Reason = d.Reason
			return s.rows[i], nil
		}
	}
	return types.ApprovalRequest{}, types.ErrDuplicatePendingApproval
}

func (s *sweepStore) Record(_ context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, ev)
	return nil
}

func (s *sweepStore) stateOf(id uuid.UUID) types.ApprovalState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			return r.State
		}
	}
	return ""
}

// countingExpiry records what the sweep reported.
type countingExpiry struct {
	mu sync.Mutex
	n  int
}

func (c *countingExpiry) RecordCredentialReauthExpired(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += n
}

func (c *countingExpiry) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestRunApprovalSweeper_CountsCredentialReauthExpiriesAtTheTransition(t *testing.T) {
	stale := time.Now().UTC().Add(-48 * time.Hour)
	reauth := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: stale,
		RequestedScope: json.RawMessage(`{"owner":"alice@corp.example"}`),
	}
	egressRow := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
		State: types.ApprovalPending, RequestedAt: stale,
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
	}
	fresh := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(`{"owner":"bob@corp.example"}`),
	}
	st := &sweepStore{rows: []types.ApprovalRequest{reauth, egressRow, fresh}}
	counter := &countingExpiry{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runApprovalSweeper(ctx, st, 5*time.Millisecond, time.Hour, counter)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for st.stateOf(reauth.ID) != types.ApprovalExpired {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the sweep never expired the stale credential_reauth row (state=%q)", st.stateOf(reauth.ID))
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	// The ROW moved — the transition really happened.
	if got := st.stateOf(reauth.ID); got != types.ApprovalExpired {
		t.Fatalf("the stale re-auth row is %s, want EXPIRED", got)
	}
	// …and the counter saw exactly it: one, not the egress row beside it, and
	// not the fresh one the cutoff protects.
	if got := counter.total(); got != 1 {
		t.Errorf("the sweep reported %d credential_reauth expiries, want exactly 1 "+
			"(the stale re-auth row only — not the egress row it swept alongside, not the fresh one)", got)
	}
	if got := st.stateOf(fresh.ID); got != types.ApprovalPending {
		t.Errorf("a request newer than the cutoff was expired: %s", got)
	}
	if got := st.stateOf(egressRow.ID); got != types.ApprovalExpired {
		t.Errorf("the egress row beside it was not swept (%s) — the sweep must still do its own job", got)
	}
}

// A sweep with nothing of this kind reports nothing: the counter must not move
// on every tick of an idle deployment.
func TestRunApprovalSweeper_ReportsNothingWhenNoReauthExpires(t *testing.T) {
	st := &sweepStore{rows: []types.ApprovalRequest{{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC().Add(-48 * time.Hour),
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
	}}}
	counter := &countingExpiry{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	runApprovalSweeper(ctx, st, 5*time.Millisecond, time.Hour, counter)
	if got := counter.total(); got != 0 {
		t.Errorf("the sweep reported %d re-auth expiries with none present", got)
	}
}
