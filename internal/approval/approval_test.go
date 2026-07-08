// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── In-memory fake store ────────────────────────────────────────────────────

type fakeStore struct {
	mu      sync.Mutex
	records []types.ApprovalRequest
	audit   []types.AuditEvent
}

func (f *fakeStore) CreateApproval(_ context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, a)
	return a, nil
}

func (f *fakeStore) GetApproval(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.records {
		if a.ID == id {
			return a, nil
		}
	}
	return types.ApprovalRequest{}, approval.ErrAlreadyDecided // sentinel not ideal, but unused in current tests
}

func (f *fakeStore) ListApprovals(_ context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.ApprovalRequest
	for _, a := range f.records {
		if stateFilter == "" || a.State == stateFilter {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) DecideApproval(_ context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.records {
		if a.ID == id {
			if a.State != types.ApprovalPending {
				return types.ApprovalRequest{}, approval.ErrAlreadyDecided
			}
			now := time.Now().UTC()
			f.records[i].State = state
			f.records[i].DecidedAt = &now
			f.records[i].DecidedBy = decidedBy
			f.records[i].Reason = reason
			return f.records[i], nil
		}
	}
	return types.ApprovalRequest{}, approval.ErrAlreadyDecided
}

func (f *fakeStore) Record(_ context.Context, ev types.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, ev)
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func newReq(runID uuid.UUID, kind types.ApprovalKind, scope json.RawMessage) types.ApprovalRequest {
	return types.ApprovalRequest{
		RunID:          runID,
		Kind:           kind,
		RequestedScope: scope,
	}
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestRequestApproval_Creates(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	scope := json.RawMessage(`{"host":"api.github.com"}`)

	got, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID == uuid.Nil {
		t.Error("expected non-nil ID")
	}
	if got.State != types.ApprovalPending {
		t.Errorf("want PENDING, got %s", got.State)
	}
	if len(st.records) != 1 {
		t.Errorf("expected 1 record, got %d", len(st.records))
	}
}

func TestRequestApproval_Dedup(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	scope := json.RawMessage(`{"host":"api.github.com"}`)

	first, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	// Same run+kind+scope: must return the existing pending approval.
	second, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("dedup failed: got two different IDs %s vs %s", first.ID, second.ID)
	}
	if len(st.records) != 1 {
		t.Errorf("expected exactly 1 record after dedup, got %d", len(st.records))
	}
}

func TestRequestApproval_DifferentScopes_BothCreated(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	scopeA := json.RawMessage(`{"host":"api.github.com"}`)
	scopeB := json.RawMessage(`{"host":"registry.npmjs.org"}`)

	a, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scopeA))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scopeB))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.ID == b.ID {
		t.Error("different scopes should produce different approvals")
	}
	if len(st.records) != 2 {
		t.Errorf("expected 2 records, got %d", len(st.records))
	}
}

func TestDecide_Approve(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	result, err := approval.Decide(ctx, st, ap.ID, true, types.ActorHuman, "alice@example.com", "looks good")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.State != types.ApprovalApproved {
		t.Errorf("want APPROVED, got %s", result.State)
	}
	if result.DecidedBy != "alice@example.com" {
		t.Errorf("want decided_by=alice@example.com, got %s", result.DecidedBy)
	}
	// Audit event must have been emitted.
	if len(st.audit) == 0 {
		t.Error("expected at least one audit event")
	}
	if st.audit[0].ActorType != types.ActorHuman {
		t.Errorf("want actor_type=human, got %s", st.audit[0].ActorType)
	}
	if st.audit[0].Action != "approval.decide" {
		t.Errorf("want action=approval.decide, got %s", st.audit[0].Action)
	}
}

// TestDecide_AdminTokenRecordsAsSystem proves the FIX #10 completion: when the
// decider is a bare admin-token caller (ActorSystem, "admin-token"), the
// approval.decide audit event records actor_type=system — NOT human. Auditing a
// tokened decision as a named human would break attribution honesty (invariant
// 4/6): "who approved this credential" must not claim a person when only the
// shared token acted.
func TestDecide_AdminTokenRecordsAsSystem(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	if _, err := approval.Decide(ctx, st, ap.ID, true, types.ActorSystem, "admin-token", "ok"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(st.audit) == 0 {
		t.Fatal("expected an audit event")
	}
	if st.audit[0].ActorType != types.ActorSystem {
		t.Errorf("want actor_type=system for an admin-token decision, got %s", st.audit[0].ActorType)
	}
	if st.audit[0].Actor != "admin-token" {
		t.Errorf("want actor=admin-token, got %s", st.audit[0].Actor)
	}
}

func TestDecide_Deny(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	result, err := approval.Decide(ctx, st, ap.ID, false, types.ActorHuman, "bob@example.com", "too risky")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.State != types.ApprovalDenied {
		t.Errorf("want DENIED, got %s", result.State)
	}
}

func TestDecide_AlreadyDecided(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	_, _ = approval.Decide(ctx, st, ap.ID, true, types.ActorHuman, "alice", "ok")
	// Second decision on the same approval must fail.
	_, err := approval.Decide(ctx, st, ap.ID, false, types.ActorHuman, "bob", "changed my mind")
	if !isAlreadyDecided(err) {
		t.Errorf("expected ErrAlreadyDecided, got %v", err)
	}
}

func TestExpireStale(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	// Create two approvals: one old, one recent.
	old, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"old.example.com"}`)))
	// Back-date the old approval by manually setting its requested_at.
	for i, r := range st.records {
		if r.ID == old.ID {
			st.records[i].RequestedAt = time.Now().UTC().Add(-10 * time.Hour)
		}
	}

	_, _ = approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"new.example.com"}`)))

	expired, err := approval.ExpireStale(ctx, st, 5*time.Hour)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if expired != 1 {
		t.Errorf("expected 1 expired, got %d", expired)
	}

	// Check the audit log.
	var expireEvents int
	for _, ev := range st.audit {
		if ev.Action == "approval.expire" {
			expireEvents++
		}
	}
	if expireEvents != 1 {
		t.Errorf("expected 1 expire audit event, got %d", expireEvents)
	}
}

func TestExpireStale_AlreadyDecidedRace(t *testing.T) {
	// If a concurrent Decide wins, ExpireStale should not error.
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"x.com"}`)))
	// Back-date.
	for i := range st.records {
		st.records[i].RequestedAt = time.Now().UTC().Add(-10 * time.Hour)
	}
	// Approve concurrently.
	_, _ = approval.Decide(ctx, st, ap.ID, true, types.ActorHuman, "human", "ok")

	// ExpireStale should skip the already-decided record gracefully.
	expired, err := approval.ExpireStale(ctx, st, 5*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Zero expired because the approval was already decided.
	if expired != 0 {
		t.Errorf("expected 0 expired, got %d", expired)
	}
}

// ─── Helper ──────────────────────────────────────────────────────────────────

func isAlreadyDecided(err error) bool {
	return err != nil && err == approval.ErrAlreadyDecided
}
