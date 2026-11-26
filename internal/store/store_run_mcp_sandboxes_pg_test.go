// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for run_mcp_sandboxes: every row write that checks the run checks it in the same
// statement, and a row another writer moved answers false. Guarded by WARDYN_TEST_PG; skipped cleanly when
// unset.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpSandboxRun is a run in state with one MCP sandbox row at ordinal 0, set to rowState.
func mcpSandboxRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state types.RunState, rowState string) uuid.UUID {
	t.Helper()
	r := newRun(state)
	persistRun(t, ctx, pool, r)
	pg := store.NewPG(pool)
	if err := pg.InsertRunMCPSandbox(ctx, store.RunMCPSandbox{RunID: r.ID, ServerKey: "files", CPTokenJTI: "jti-1",
		RelayAddr: "10.0.0.3:8443", Reserved: types.RunnerAmounts{CPUMillis: 500, MemoryMiB: 512}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	setMCPSandboxState(t, ctx, pool, r.ID, rowState)
	return r.ID
}

func setMCPSandboxState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, state string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE run_mcp_sandboxes SET state = $2 WHERE run_id = $1 AND ordinal = 0`, runID, state); err != nil {
		t.Fatalf("set row state: %v", err)
	}
}

func markRunLost(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("mark lost: %v", err)
	}
}

func mcpSandboxRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) store.RunMCPSandbox {
	t.Helper()
	rows, err := store.NewPG(pool).ListRunMCPSandboxes(ctx, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %d rows, %v; want 1", len(rows), err)
	}
	return rows[0]
}

// TestMarkMCPSandboxReadyIfLive: the restart's write applies on a RUNNING run with lost_at null, and answers
// false on a KILLED run, on a RUNNING run with lost_at set, and on a stopped row.
func TestMarkMCPSandboxReadyIfLive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	live := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxRestarting)
	if ok, err := pg.MarkMCPSandboxReadyIfLive(ctx, live, 0, "ref-1"); err != nil || !ok {
		t.Fatalf("live run: applied %v, %v; want true", ok, err)
	}
	if m := mcpSandboxRow(t, ctx, pool, live); m.State != store.MCPSandboxReady || m.SandboxRef != "ref-1" {
		t.Errorf("live row = %s %q, want ready ref-1", m.State, m.SandboxRef)
	}

	killed := mcpSandboxRun(t, ctx, pool, types.RunKilled, store.MCPSandboxRestarting)
	lost := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxRestarting)
	markRunLost(t, ctx, pool, lost)
	stopped := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStopped)
	for name, id := range map[string]uuid.UUID{"killed run": killed, "lost run": lost, "stopped row": stopped} {
		before := mcpSandboxRow(t, ctx, pool, id)
		if ok, err := pg.MarkMCPSandboxReadyIfLive(ctx, id, 0, "ref-2"); err != nil || ok {
			t.Errorf("%s: applied %v, %v; want false", name, ok, err)
		}
		if after := mcpSandboxRow(t, ctx, pool, id); after.State != before.State || after.SandboxRef != "" {
			t.Errorf("%s: row moved to %s %q", name, after.State, after.SandboxRef)
		}
	}
}

// TestReopenAndKeepMCPSandboxIfLive: the revive's reopen takes a row from any of the five states on a live
// run and refuses a killed or lost run and a row written after the read; dispatch's started write needs a
// STARTING run; the fails and the keep leave a row another writer stopped as it is; a token renew between the
// read and the reopen does not make the reopen false.
func TestReopenAndKeepMCPSandboxIfLive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	until := time.Now().Add(time.Minute)

	for _, st := range []string{store.MCPSandboxStopped, store.MCPSandboxFailed, store.MCPSandboxRestarting,
		store.MCPSandboxReady, store.MCPSandboxStarting} {
		id := mcpSandboxRun(t, ctx, pool, types.RunRunning, st)
		read := mcpSandboxRow(t, ctx, pool, id)
		// A renew lands between the read and the reopen: it leaves updated_at as it was.
		if ok, err := pg.RotateMCPSandboxToken(ctx, id, 0, "jti-1", "jti-renewed", until); st != store.MCPSandboxStopped && (err != nil || !ok) {
			t.Fatalf("%s: rotate applied %v, %v; want true", st, ok, err)
		}
		if ok, err := pg.ReopenMCPSandboxIfLive(ctx, id, 0, read.UpdatedAt, "jti-2", until); err != nil || !ok {
			t.Fatalf("%s: reopen applied %v, %v; want true", st, ok, err)
		}
		m := mcpSandboxRow(t, ctx, pool, id)
		if m.State != store.MCPSandboxStarting || m.CPTokenJTI != "jti-2" || m.PrevCPTokenJTI == "" || m.LastCause != "" || m.StoppedAt != nil {
			t.Errorf("%s: reopened row = %+v", st, m)
		}
		// The keep then applies while the row is starting.
		if ok, err := pg.KeepMCPSandboxStartingIfLive(ctx, id, 0, "ref-3"); err != nil || !ok {
			t.Errorf("%s: keep applied %v, %v; want true", st, ok, err)
		}
	}

	killed := mcpSandboxRun(t, ctx, pool, types.RunKilled, store.MCPSandboxStopped)
	lost := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStopped)
	markRunLost(t, ctx, pool, lost)
	for name, id := range map[string]uuid.UUID{"killed run": killed, "lost run": lost} {
		read := mcpSandboxRow(t, ctx, pool, id)
		if ok, err := pg.ReopenMCPSandboxIfLive(ctx, id, 0, read.UpdatedAt, "jti-2", until); err != nil || ok {
			t.Errorf("%s: reopen applied %v, %v; want false", name, ok, err)
		}
	}
	// A person's Stop between the read and the reopen moves updated_at, so the reopen is false.
	moved := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxReady)
	read := mcpSandboxRow(t, ctx, pool, moved)
	if ok, err := pg.StopMCPSandboxRow(ctx, moved, 0, "person"); err != nil || !ok {
		t.Fatalf("stop applied %v, %v; want true", ok, err)
	}
	if ok, err := pg.ReopenMCPSandboxIfLive(ctx, moved, 0, read.UpdatedAt, "jti-2", until); err != nil || ok {
		t.Errorf("reopen after a Stop: applied %v, %v; want false", ok, err)
	}
	if m := mcpSandboxRow(t, ctx, pool, moved); m.State != store.MCPSandboxStopped || m.LastCause != "person" || m.StoppedAt == nil {
		t.Errorf("stopped row = %+v", m)
	}

	starting := mcpSandboxRun(t, ctx, pool, types.RunStarting, store.MCPSandboxStarting)
	running := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStarting)
	if ok, err := pg.MarkMCPSandboxStartedIfStarting(ctx, starting, 0, "ref-1"); err != nil || !ok {
		t.Errorf("started on a STARTING run: applied %v, %v; want true", ok, err)
	}
	if ok, err := pg.MarkMCPSandboxStartedIfStarting(ctx, running, 0, "ref-1"); err != nil || ok {
		t.Errorf("started on a RUNNING run: applied %v, %v; want false", ok, err)
	}
	if ok, err := pg.FailMCPSandboxIfUp(ctx, starting, 0, "readiness"); err != nil || !ok {
		t.Errorf("fail a ready row: applied %v, %v; want true", ok, err)
	}
	if m := mcpSandboxRow(t, ctx, pool, starting); m.State != store.MCPSandboxFailed || m.LastCause != "readiness" {
		t.Errorf("failed row = %s %q", m.State, m.LastCause)
	}
	if ok, err := pg.FailMCPSandboxIfUp(ctx, moved, 0, "readiness"); err != nil || ok {
		t.Errorf("fail a stopped row: applied %v, %v; want false", ok, err)
	}

	keepStopped := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStopped)
	keepKilled := mcpSandboxRun(t, ctx, pool, types.RunKilled, store.MCPSandboxStarting)
	keepLost := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStarting)
	markRunLost(t, ctx, pool, keepLost)
	for name, id := range map[string]uuid.UUID{"stopped row": keepStopped, "killed run": keepKilled, "lost run": keepLost} {
		if ok, err := pg.KeepMCPSandboxStartingIfLive(ctx, id, 0, "ref-4"); err != nil || ok {
			t.Errorf("keep on a %s: applied %v, %v; want false", name, ok, err)
		}
	}
	if ok, err := pg.FailMCPSandboxIfStarting(ctx, keepStopped, 0, "state_write"); err != nil || ok {
		t.Errorf("fail-if-starting on a stopped row: applied %v, %v; want false", ok, err)
	}
	if m := mcpSandboxRow(t, ctx, pool, keepStopped); m.State != store.MCPSandboxStopped {
		t.Errorf("stopped row moved to %s", m.State)
	}
}

// TestRestartAndRecordMCPSandbox: a restart moves a ready, failed, restarting or stopped row of a RUNNING run
// to restarting with the fresh token and its window, and answers false on a starting row, on a KILLED or lost
// run, and once the row moved after the read; the created record lands on a starting or restarting row and
// never on a stopped one.
func TestRestartAndRecordMCPSandbox(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	window := time.Now().UTC().Truncate(time.Second)
	restart := func(read store.RunMCPSandbox) store.MCPSandboxRestart {
		return store.MCPSandboxRestart{ReadAt: read.UpdatedAt, JTI: "jti-r", PrevUntil: window.Add(time.Minute), Restarts: 2, WindowStart: window}
	}
	for _, st := range []string{store.MCPSandboxReady, store.MCPSandboxFailed, store.MCPSandboxRestarting, store.MCPSandboxStopped} {
		id := mcpSandboxRun(t, ctx, pool, types.RunRunning, st)
		if ok, err := pg.RestartMCPSandboxIfLive(ctx, id, 0, restart(mcpSandboxRow(t, ctx, pool, id))); err != nil || !ok {
			t.Fatalf("%s: restart applied %v, %v; want true", st, ok, err)
		}
		m := mcpSandboxRow(t, ctx, pool, id)
		if m.State != store.MCPSandboxRestarting || m.CPTokenJTI != "jti-r" || m.Restarts != 2 || !m.RestartWindowStart.Equal(window) || m.StoppedAt != nil {
			t.Errorf("%s: restarted row = %+v", st, m)
		}
		if ok, err := pg.RecordMCPSandboxCreated(ctx, id, 0, store.MCPSandboxCreated{RelayAddr: "172.30.0.2:3130", PolicyDigest: "d", ProxyConfigSealed: []byte{1}}); err != nil || !ok {
			t.Errorf("%s: record applied %v, %v; want true", st, ok, err)
		}
		if m := mcpSandboxRow(t, ctx, pool, id); m.RelayAddr != "172.30.0.2:3130" || m.PolicyDigest != "d" {
			t.Errorf("%s: recorded row = %+v", st, m)
		}
	}
	killed := mcpSandboxRun(t, ctx, pool, types.RunKilled, store.MCPSandboxReady)
	lost := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxReady)
	markRunLost(t, ctx, pool, lost)
	starting := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxStarting)
	for name, id := range map[string]uuid.UUID{"killed run": killed, "lost run": lost, "starting row": starting} {
		if ok, err := pg.RestartMCPSandboxIfLive(ctx, id, 0, restart(mcpSandboxRow(t, ctx, pool, id))); err != nil || ok {
			t.Errorf("%s: restart applied %v, %v; want false", name, ok, err)
		}
	}
	moved := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxReady)
	read := mcpSandboxRow(t, ctx, pool, moved)
	if ok, err := pg.StopMCPSandboxRow(ctx, moved, 0, "person"); err != nil || !ok {
		t.Fatalf("stop applied %v, %v; want true", ok, err)
	}
	if ok, err := pg.RestartMCPSandboxIfLive(ctx, moved, 0, restart(read)); err != nil || ok {
		t.Errorf("restart after a Stop: applied %v, %v; want false", ok, err)
	}
	if ok, err := pg.RecordMCPSandboxCreated(ctx, moved, 0, store.MCPSandboxCreated{RelayAddr: "172.30.0.2:3130"}); err != nil || ok {
		t.Errorf("record on a stopped row: applied %v, %v; want false", ok, err)
	}
	// A restart whose create fails fails its restarting row.
	restarting := mcpSandboxRun(t, ctx, pool, types.RunRunning, store.MCPSandboxRestarting)
	if ok, err := pg.FailMCPSandboxIfUp(ctx, restarting, 0, "start"); err != nil || !ok || mcpSandboxRow(t, ctx, pool, restarting).State != store.MCPSandboxFailed {
		t.Errorf("fail a restarting row: applied %v, %v; want failed", ok, err)
	}
}
