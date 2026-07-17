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
	"encoding/json"
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
