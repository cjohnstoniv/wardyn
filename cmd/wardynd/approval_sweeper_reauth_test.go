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

// The sweeper is where a credential_reauth request actually expires, and this
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
	// An AWS SSO re-auth's own raise shape (injection_awssso.go): mechanism +
	// credential_source + owner, no lane. This is the ONE shape the metric's
	// HELP promises, so it is the only row the counter should see.
	reauth := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: stale,
		RequestedScope: json.RawMessage(
			`{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"alice@corp.example"}`),
	}
	// An Azure DevOps sign-in's own raise shape (injection_ado_signin.go):
	// lane + mechanism, no credential_source. Also credential_reauth, and also
	// stale, so the sweep must still expire it — but it is NOT the metric's
	// population, and must not move the counter (#971).
	adoSignIn := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: stale,
		RequestedScope: json.RawMessage(
			`{"lane":"azure_devops","mechanism":"entra_signin","owner":"eve@corp.example","provider_id":"p"}`),
	}
	egressRow := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
		State: types.ApprovalPending, RequestedAt: stale,
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
	}
	fresh := types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		RequestedScope: json.RawMessage(
			`{"mechanism":"bedrock_sso","credential_source":"per_user","owner":"bob@corp.example"}`),
	}
	st := &sweepStore{rows: []types.ApprovalRequest{reauth, adoSignIn, egressRow, fresh}}
	counter := &countingExpiry{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runApprovalSweeper(ctx, st, 5*time.Millisecond, time.Hour, counter)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for st.stateOf(reauth.ID) != types.ApprovalExpired || st.stateOf(adoSignIn.ID) != types.ApprovalExpired {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the sweep never expired the stale rows (aws=%q, ado=%q)",
				st.stateOf(reauth.ID), st.stateOf(adoSignIn.ID))
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	// Both ROWS moved — the state machine expires every stale credential_reauth
	// row regardless of lane; only the METRIC is AWS-SSO-only.
	if got := st.stateOf(reauth.ID); got != types.ApprovalExpired {
		t.Fatalf("the stale AWS SSO re-auth row is %s, want EXPIRED", got)
	}
	if got := st.stateOf(adoSignIn.ID); got != types.ApprovalExpired {
		t.Fatalf("the stale Azure DevOps sign-in row is %s, want EXPIRED", got)
	}
	// …and the counter saw exactly the AWS SSO row: one, not the Azure DevOps
	// sign-in beside it (also credential_reauth, but not this metric's
	// population), not the egress row, and not the fresh one the cutoff
	// protects.
	if got := counter.total(); got != 1 {
		t.Errorf("the sweep reported %d credential_reauth expiries, want exactly 1 "+
			"(the stale AWS SSO row only — not the Azure DevOps sign-in it swept alongside, "+
			"not the egress row, not the fresh one)", got)
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
