// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the Record Mode store primitives — the SQL-level
// atomicity/CAS semantics the api layer's concurrency safety rests on.
// Guarded by WARDYN_TEST_PG (see store_pg_test.go).
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

func TestPG_RecordResultUpsertAndStatusCAS(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	ws, err := store.NewPG(pool).CreateWorkspace(ctx, types.Workspace{
		// Name is UNIQUE: a fixed literal makes the test pass once and fail on
		// every later run against the same database (and `make release-check`
		// runs this suite twice — unit lane, then docker lane).
		ID: uuid.New(), Name: "rec-pg-" + uuid.NewString(), Kind: types.WorkspaceKindLocalDir,
		Source: "/tmp/rec-pg-" + uuid.NewString(), Status: types.WorkspaceScanned,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	entry := func(status string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"run_id": uuid.New(), "mode": "auto", "status": status})
		return b
	}
	statusOf := func(blob json.RawMessage, task string) string {
		m := map[string]struct {
			Status string `json:"status"`
		}{}
		_ = json.Unmarshal(blob, &m)
		return m[task].Status
	}

	// Unconditional upsert creates the entry; a second task's upsert must not
	// disturb the first (per-key merge, not whole-map replace).
	if _, applied, err := store.NewPG(pool).SetWorkspaceRecordResult(ctx, ws.ID, "build", entry("recording"), ""); err != nil || !applied {
		t.Fatalf("initial upsert: applied=%v err=%v", applied, err)
	}
	got, applied, err := store.NewPG(pool).SetWorkspaceRecordResult(ctx, ws.ID, "test", entry("recording"), "")
	if err != nil || !applied {
		t.Fatalf("second-task upsert: applied=%v err=%v", applied, err)
	}
	if statusOf(got.RecordResults, "build") != "recording" || statusOf(got.RecordResults, "test") != "recording" {
		t.Fatalf("per-key merge lost an entry: %s", got.RecordResults)
	}

	// CAS: recording→recorded applies; a stale write guarded on `recording`
	// must then no-op (the late-upload / double-capture protection).
	if _, applied, err = store.NewPG(pool).SetWorkspaceRecordResult(ctx, ws.ID, "build", entry("recorded"), "recording"); err != nil || !applied {
		t.Fatalf("recording→recorded CAS: applied=%v err=%v", applied, err)
	}
	got, applied, err = store.NewPG(pool).SetWorkspaceRecordResult(ctx, ws.ID, "build", entry("recording"), "recording")
	if err != nil {
		t.Fatalf("guarded stale write: %v", err)
	}
	if applied {
		t.Fatal("stale write guarded on `recording` applied over a completed capture")
	}
	if statusOf(got.RecordResults, "build") != "recorded" {
		t.Fatalf("completed capture reverted: %s", got.RecordResults)
	}

	// Missing workspace is ErrNotFound, not a silent guard-miss.
	if _, _, err := store.NewPG(pool).SetWorkspaceRecordResult(ctx, uuid.New(), "build", entry("recording"), "recording"); err == nil {
		t.Fatal("guarded write against a missing workspace must error")
	}
}

func TestPG_ClaimAndClearActiveRunCAS(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	ws, err := store.NewPG(pool).CreateWorkspace(ctx, types.Workspace{
		// Unique per run — see the note in the sibling test above.
		ID: uuid.New(), Name: "claim-pg-" + uuid.NewString(), Kind: types.WorkspaceKindLocalDir,
		Source: "/tmp/claim-pg-" + uuid.NewString(), Status: types.WorkspaceScanned,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	runA, runB := uuid.New(), uuid.New()
	// Two launches that both observed a free slot: exactly one CAS wins.
	if _, claimed, err := store.NewPG(pool).ClaimWorkspaceActiveRun(ctx, ws.ID, runA, nil); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := store.NewPG(pool).ClaimWorkspaceActiveRun(ctx, ws.ID, runB, nil); err != nil || claimed {
		t.Fatalf("second claim from the same observation must lose: claimed=%v err=%v", claimed, err)
	}
	// Clear is conditional on the pointer still being ours.
	if cleared, err := store.NewPG(pool).ClearWorkspaceActiveRun(ctx, ws.ID, runB); err != nil || cleared {
		t.Fatalf("loser's clear must no-op: cleared=%v err=%v", cleared, err)
	}
	if cleared, err := store.NewPG(pool).ClearWorkspaceActiveRun(ctx, ws.ID, runA); err != nil || !cleared {
		t.Fatalf("owner's clear: cleared=%v err=%v", cleared, err)
	}
	// A claim expecting the (now stale) old pointer fails; expecting nil wins.
	if _, claimed, err := store.NewPG(pool).ClaimWorkspaceActiveRun(ctx, ws.ID, runB, &runA); err != nil || claimed {
		t.Fatalf("claim with stale expectation must lose: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := store.NewPG(pool).ClaimWorkspaceActiveRun(ctx, ws.ID, runB, nil); err != nil || !claimed {
		t.Fatalf("claim of the freed slot: claimed=%v err=%v", claimed, err)
	}
}
