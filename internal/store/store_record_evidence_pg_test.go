// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// The audit position a confined capture reads through, the probe for a row that lands past it, and the swap
// that writes a verdict taken again only over the entry it was read from.
func TestPG_RecordEvidencePositionProbeAndSwap(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	runID := uuid.New()

	if seq, err := pg.RunAuditSeq(ctx, runID); err != nil || seq != 0 {
		t.Fatalf("a run with no rows: seq %d, err %v; want 0", seq, err)
	}
	insert := func(action string) {
		t.Helper()
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorSystem,
			Actor: "record-evidence-probe", Action: action, Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("insert %s: %v", action, err)
		}
	}
	insert("egress.allow")
	pos, err := pg.RunAuditSeq(ctx, runID)
	if err != nil || pos == 0 {
		t.Fatalf("after one row: seq %d, err %v; want its seq", pos, err)
	}
	egress := store.AuditFilter{ActionPrefix: "egress."}
	probe := func(id uuid.UUID, after int64, f store.AuditFilter) bool {
		t.Helper()
		found, err := pg.HasRunAuditEventAfter(ctx, id, after, f)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return found
	}
	if probe(runID, pos, egress) {
		t.Fatal("nothing has landed past the position, yet the probe found a row")
	}
	insert("workspace.record")
	if probe(runID, pos, egress) {
		t.Fatal("a row past the position that the filter does not match was found")
	}
	insert("egress.deny")
	if !probe(runID, pos, egress) {
		t.Fatal("an egress row past the position was not found")
	}
	if probe(uuid.New(), 0, egress) {
		t.Fatal("another run's rows were found")
	}

	now := time.Now().UTC()
	ws, err := pg.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "rec-evidence-pg-" + uuid.NewString(),
		Sources: []types.WorkspaceSource{{
			Type: types.WorkspaceSourceTypeLocalDir, Path: "/tmp/rec-evidence-pg-" + uuid.NewString(), Target: "/home/agent/work",
		}},
		Status:    types.WorkspaceScanned,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	entry := func(clean bool) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"run_id": runID, "status": "recorded", "confined": true, "clean": clean, "evidence_seq": pos})
		return b
	}
	for _, task := range []string{"verify:build", "build"} {
		if _, applied, err := pg.SetWorkspaceRecordResult(ctx, ws.ID, task, entry(true), ""); err != nil || !applied {
			t.Fatalf("seed %s: applied %v, err %v", task, applied, err)
		}
	}
	stored := func(task string) json.RawMessage {
		t.Helper()
		got, err := pg.GetWorkspace(ctx, ws.ID)
		if err != nil {
			t.Fatalf("get workspace: %v", err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(got.RecordResults, &m); err != nil {
			t.Fatalf("decode record_results: %v", err)
		}
		return m[task]
	}
	read := stored("verify:build") // as the database renders it: its own key order and spacing
	if swapped, err := pg.SwapWorkspaceRecordResult(ctx, ws.ID, "verify:build", read, entry(false)); err != nil || !swapped {
		t.Fatalf("swap over the entry as read: swapped %v, err %v", swapped, err)
	}
	if swapped, err := pg.SwapWorkspaceRecordResult(ctx, ws.ID, "verify:build", read, entry(true)); err != nil || swapped {
		t.Fatalf("swap over an entry that has changed since it was read: swapped %v, err %v; want no write", swapped, err)
	}
	var after struct {
		Clean bool `json:"clean"`
	}
	if err := json.Unmarshal(stored("verify:build"), &after); err != nil || after.Clean {
		t.Fatalf("verify:build after the swaps: %s; want the first swap's verdict", stored("verify:build"))
	}
	if err := json.Unmarshal(stored("build"), &after); err != nil || !after.Clean {
		t.Fatalf("a swap of one task changed another: build is %s", stored("build"))
	}
}
