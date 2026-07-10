// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the store layer. Guarded by WARDYN_TEST_PG.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_DecideApproval_SingleTransition(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// Create a minimal run first (FK requirement).
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
	if _, err := store.CreateRun(ctx, pool, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Create an approval.
	apID := uuid.New()
	ap := types.ApprovalRequest{
		ID:             apID,
		RunID:          runID,
		Kind:           types.ApprovalCredential,
		RequestedScope: json.RawMessage(`{"kind":"github_token"}`),
		State:          types.ApprovalPending,
		RequestedAt:    time.Now().UTC(),
	}
	if _, err := store.CreateApproval(ctx, pool, ap); err != nil {
		t.Fatalf("create approval: %v", err)
	}

	// First decide: APPROVE.
	result, err := store.DecideApproval(ctx, pool, apID, types.ApprovalApproved, "alice", "ok")
	if err != nil {
		t.Fatalf("first decide: %v", err)
	}
	if result.State != types.ApprovalApproved {
		t.Errorf("want APPROVED, got %s", result.State)
	}

	// Second decide: must return ErrAlreadyDecided.
	_, err = store.DecideApproval(ctx, pool, apID, types.ApprovalDenied, "bob", "changed mind")
	if err == nil {
		t.Fatal("expected ErrAlreadyDecided, got nil")
	}
	if err != store.ErrAlreadyDecided {
		t.Errorf("expected store.ErrAlreadyDecided, got %v", err)
	}
}

func TestPG_AuditAppendOnly_TriggerRejects(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// Insert a legitimate audit event.
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "test-component",
		Action:    "run.create",
		Outcome:   "success",
	}
	if err := store.InsertAuditEvent(ctx, pool, ev); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}

	// Attempt UPDATE on audit_events: Postgres trigger must reject it.
	_, updateErr := pool.Exec(ctx,
		`UPDATE audit_events SET outcome='failure' WHERE id=$1`, ev.ID)
	if updateErr == nil {
		t.Fatal("expected UPDATE on audit_events to fail (append-only trigger), got nil")
	}

	// Attempt DELETE: also blocked.
	_, deleteErr := pool.Exec(ctx,
		`DELETE FROM audit_events WHERE id=$1`, ev.ID)
	if deleteErr == nil {
		t.Fatal("expected DELETE on audit_events to fail (append-only trigger), got nil")
	}

	// Attempt TRUNCATE: the statement-level guard (migration 0004) must reject
	// it. Without it, TRUNCATE silently wipes the entire append-only log.
	_, truncErr := pool.Exec(ctx, `TRUNCATE audit_events`)
	if truncErr == nil {
		t.Fatal("expected TRUNCATE on audit_events to fail (append-only trigger), got nil")
	}
}
