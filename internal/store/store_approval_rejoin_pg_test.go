// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ApprovalRejoinStampsOnlyTheRunsPendingRow: a raise that deduplicates
// onto a PENDING row stamps last_rejoined_at (0193), which every approvals read
// carries, so approval.Hold measures the re-entered hold from it. Another run,
// another kind, or a decided row is never stamped.
func TestPG_ApprovalRejoinStampsOnlyTheRunsPendingRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}
	runID := uuid.New()
	if _, err := pg.CreateRun(ctx, types.AgentRun{
		ID: runID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), CreatedBy: "test-user",
		Agent: "claude-code", Repo: "test/repo", Task: "t", ConfinementClass: types.CC2, State: types.RunPending,
		SPIFFEID: "spiffe://test/agent-run/" + runID.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	req := types.ApprovalRequest{RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"pkg.example.com","mode":"wait_for_review"}`)}
	first, err := approval.RequestApproval(ctx, st, req)
	if err != nil || first.LastRejoinedAt != nil {
		t.Fatalf("raise: %+v, %v (a fresh row has no rejoin)", first, err)
	}
	second, err := approval.RequestApproval(ctx, st, req)
	if err != nil || second.ID != first.ID || second.LastRejoinedAt == nil {
		t.Fatalf("dedup hit: %+v, %v; want the same row with last_rejoined_at set", second, err)
	}
	got, err := pg.GetApproval(ctx, first.ID)
	if err != nil || got.LastRejoinedAt == nil || !got.LastRejoinedAt.Equal(*second.LastRejoinedAt) {
		t.Fatalf("read back: %+v, %v; want last_rejoined_at %v", got, err, second.LastRejoinedAt)
	}
	for name, c := range map[string]struct {
		run  uuid.UUID
		kind types.ApprovalKind
	}{"another run": {uuid.New(), types.ApprovalEgressDomain}, "another kind": {runID, types.ApprovalToolCall}} {
		if _, ok, err := pg.StampApprovalRejoin(ctx, first.ID, c.run, c.kind, time.Now().UTC()); err != nil || ok {
			t.Errorf("%s: stamped = %v, %v; want not stamped", name, ok, err)
		}
	}
	if _, err := pg.DecideApproval(ctx, first.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "u"}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, ok, err := pg.StampApprovalRejoin(ctx, first.ID, runID, types.ApprovalEgressDomain, time.Now().UTC()); err != nil || ok {
		t.Errorf("decided row: stamped = %v, %v; want not stamped", ok, err)
	}
}
