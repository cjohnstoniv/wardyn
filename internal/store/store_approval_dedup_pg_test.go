// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Concurrency integration test for the approval dedup race (migration 0022:
// approvals_pending_noncred_uniq). Guarded by WARDYN_TEST_PG; skipped cleanly
// when unset. Run with:
//
//	WARDYN_TEST_PG=postgres://... go test -race ./internal/store/...
package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approvalStore adapts store.PG to approval.Store. RequestApproval only calls
// ListApprovals + CreateApproval; Record is present to satisfy the interface and
// is never exercised here.
type approvalStore struct{ store.PG }

func (approvalStore) Record(context.Context, types.AuditEvent) error { return nil }

// TestPG_RequestApproval_ConcurrentDedupOneRow fires N concurrent raises of the
// SAME run+kind+scope and asserts they all resolve to a single PENDING row —
// the list-then-create dedup plus the partial unique index (0022) must collapse
// the race, never persisting duplicates.
func TestPG_RequestApproval_ConcurrentDedupOneRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := approvalStore{store.NewPG(pool)}

	// Seed the run the approvals FK-reference.
	runID := uuid.New()
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		CreatedBy:        "test-user",
		Agent:            "claude-code",
		Repo:             "test/repo",
		Task:             "test task",
		ConfinementClass: types.CC2,
		State:            types.RunPending,
		SPIFFEID:         "spiffe://test/agent-run/" + runID.String(),
		RunnerTarget:     "docker",
	}
	if _, err := store.NewPG(pool).CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	scope := json.RawMessage(`{"domain":"example.com"}`)
	mkReq := func() types.ApprovalRequest {
		return types.ApprovalRequest{
			RunID:          runID,
			Kind:           types.ApprovalEgressDomain,
			RequestedScope: scope,
		}
	}

	const n = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize the race
			got, err := approval.RequestApproval(ctx, st, mkReq())
			ids[i], errs[i] = got.ID, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("raise %d errored (concurrent dedup must not fail): %v", i, err)
		}
	}
	// Every raise must return the SAME approval id (all deduped to one winner).
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("raise %d returned id %s, want %s (all raises must dedup to one row)", i, ids[i], ids[0])
		}
	}
	// And the DB holds exactly one PENDING row for this run+kind.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM approvals WHERE run_id=$1 AND kind='egress_domain' AND state='PENDING'`,
		runID).Scan(&count); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d PENDING rows, want exactly 1 (0022 unique index must reject duplicates)", count)
	}
}

// TestPG_CreateApproval_LargeScopeKeepsDedup: a pending scope too large for a btree entry (4 KiB of
// incompressible hex, as a tool_call carrying a token-laden command is) must still insert, an identical
// second one must still be refused as a duplicate, and a different scope must insert alongside it.
func TestPG_CreateApproval_LargeScopeKeepsDedup(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	big := func() string {
		b := make([]byte, 2048)
		if _, err := rand.Read(b); err != nil {
			t.Fatalf("rand: %v", err)
		}
		return `{"cmd":"` + hex.EncodeToString(b) + `"}`
	}
	scope := big()
	if _, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalToolCall, scope)); err != nil {
		t.Fatalf("4 KiB scope must insert: %v", err)
	}
	if _, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalToolCall, scope)); !errors.Is(err, store.ErrDuplicatePending) {
		t.Fatalf("identical pending scope = %v, want ErrDuplicatePending", err)
	}
	if _, err := st.CreateApproval(ctx, pendingApproval(run.ID, types.ApprovalToolCall, big())); err != nil {
		t.Fatalf("a different scope must insert: %v", err)
	}
}

// readBarrierStore holds each raise's first dedup read until every raise has read, so every one
// reaches the insert having seen no pending row.
type readBarrierStore struct {
	approvalStore
	read   *sync.WaitGroup
	waited bool
}

func (b *readBarrierStore) ListPendingApprovalsForRuns(ctx context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error) {
	out, err := b.approvalStore.ListPendingApprovalsForRuns(ctx, runIDs)
	if !b.waited {
		b.waited = true
		b.read.Done()
		b.read.Wait()
	}
	return out, err
}

// TestPG_RequestApproval_ValueEqualSpellingsOneRow: raises whose scopes are value-equal but spell a
// number differently all read before any inserts, and still leave one PENDING row. The index hashes
// jsonb_send bytes, which keep a number's scale, so this holds only because the insert carries one
// spelling per value. A row stored with an older spelling still dedups a later raise, by the read.
func TestPG_RequestApproval_ValueEqualSpellingsOneRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	spellings := []string{"443", "443.0", "4.43e2", "4430e-1", "443.00"}
	var read, done sync.WaitGroup
	read.Add(len(spellings))
	ids := make([]uuid.UUID, len(spellings))
	errs := make([]error, len(spellings))
	for i, n := range spellings {
		done.Add(1)
		go func() {
			defer done.Done()
			st := &readBarrierStore{approvalStore: approvalStore{store.NewPG(pool)}, read: &read}
			got, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain,
				RequestedScope: json.RawMessage(`{"domain":"example.com","port":` + n + `}`)})
			ids[i], errs[i] = got.ID, err
		}()
	}
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("raise %d (%s): %v", i, spellings[i], err)
		}
		if ids[i] != ids[0] {
			t.Errorf("raise %d (%s) got approval %s, want %s", i, spellings[i], ids[i], ids[0])
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM approvals WHERE run_id=$1 AND state='PENDING'`, run.ID); n != 1 {
		t.Fatalf("got %d PENDING rows for value-equal raises, want 1", n)
	}

	other := persistRun(t, ctx, pool, newRun(types.RunRunning))
	old, err := store.NewPG(pool).CreateApproval(ctx, pendingApproval(other.ID, types.ApprovalEgressDomain, `{"domain":"example.com","port":443.0}`))
	if err != nil {
		t.Fatalf("seed a row in the old spelling: %v", err)
	}
	got, err := approval.RequestApproval(ctx, approvalStore{store.NewPG(pool)}, types.ApprovalRequest{RunID: other.ID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"domain":"example.com","port":443}`)})
	if err != nil || got.ID != old.ID {
		t.Fatalf("raise over an old-spelling row = %s, %v; want the old row %s", got.ID, err, old.ID)
	}
}
