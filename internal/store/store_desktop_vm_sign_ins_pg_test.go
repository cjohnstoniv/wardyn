// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Sign-ins in a VM's desktop (migrations 0230, 0231 and 0234) against a real Postgres. Guarded by WARDYN_TEST_PG.
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

func TestPG_DesktopVMSignIns(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.DesktopVMSignIns(ctx, run.ID); err != nil || found {
		t.Fatalf("a run with no row = %v %v", found, err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, "memory"); err != nil {
		t.Fatal(err)
	}
	// Migration 0231's default: a row written without a generation reads 0, so the first pause takes 1.
	if row, _, err := st.DesktopVMSignIns(ctx, run.ID); err != nil || row.PauseGeneration != 0 {
		t.Fatalf("a new row's pause generation = %d %v, want 0", row.PauseGeneration, err)
	}
	for want := int64(1); want <= 2; want++ {
		if gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID); err != nil || gen != want {
			t.Fatalf("next pause generation = %d %v, want %d", gen, err, want)
		}
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at, 1); err != nil || ok {
		t.Fatalf("a mark under a generation another pause has taken since = %v %v, want nothing changed", ok, err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at, 2); err != nil || !ok {
		t.Fatalf("pause = %v %v", ok, err)
	}
	if ok, _ := st.PauseDesktopVMAgent(ctx, run.ID, "bob", at.Add(time.Second), 2); ok {
		t.Fatal("a second pause changed the row")
	}
	row, found, err := st.DesktopVMSignIns(ctx, run.ID)
	if err != nil || !found || row.Store != "memory" || row.PausedBy != "alice" || row.PausedAt == nil || !row.PausedAt.Equal(at) ||
		row.PauseGeneration != 2 {
		t.Fatalf("paused row = %+v %v %v", row, found, err)
	}
	if got, err := st.PausedDesktopVMAgents(ctx); err != nil || len(got) != 1 || got[0].RunID != run.ID || got[0].PausedBy != "alice" || !got[0].PausedAt.Equal(at) ||
		got[0].PauseGeneration != 2 {
		t.Fatalf("paused agents = %+v %v, want the one paused run at generation 2", got, err)
	}
	if _, _, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at.Add(time.Second), 2); err != nil || ok {
		t.Fatalf("a resume of another pause = %v %v, want nothing cleared", ok, err)
	}
	if _, _, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at, 1); err != nil || ok {
		t.Fatalf("a resume of another generation = %v %v, want nothing cleared", ok, err)
	}
	by, pausedAt, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at, 2)
	if err != nil || !ok || by != "alice" || !pausedAt.Equal(at) {
		t.Fatalf("resume = %q %v %v %v, want the pause it cleared", by, pausedAt, ok, err)
	}
	if got, err := st.PausedDesktopVMAgents(ctx); err != nil || len(got) != 0 {
		t.Fatalf("paused agents after the resume = %+v %v, want none", got, err)
	}
	if _, _, ok, _ := st.ResumeDesktopVMAgent(ctx, run.ID, at, 2); ok {
		t.Fatal("a second resume saw the pause again")
	}
	if row, _, _ := st.DesktopVMSignIns(ctx, run.ID); row.PausedAt != nil || row.PausedBy != "" || row.PauseGeneration != 2 {
		t.Fatalf("resumed row = %+v, want no mark and the generation kept", row)
	}
	// A guest that boots again reports its store again: the mark goes, and the generation keeps counting.
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, "memory"); err != nil {
		t.Fatal(err)
	}
	if gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID); err != nil || gen != 3 {
		t.Fatalf("next pause generation after the store was recorded again = %d %v, want 3", gen, err)
	}
	none, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, none.ID, "alice", at, 0); err != nil || ok {
		t.Fatalf("pause on a store that keeps none = %v %v, want nothing changed", ok, err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "disk"); err == nil {
		t.Fatal("a store other than memory, run_disk or none was recorded")
	}

	// A macOS VM keeps its sign-ins on the run's disk: its row is recorded and its agent paused.
	mac, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, mac.ID, "run_disk"); err != nil {
		t.Fatalf("record run_disk: %v", err)
	}
	gen, err := st.NextDesktopVMPauseGeneration(ctx, mac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, mac.ID, "alice", at, gen); err != nil || !ok {
		t.Fatalf("pause on a run_disk store = %v %v, want marked", ok, err)
	}
	if row, _, err := st.DesktopVMSignIns(ctx, mac.ID); err != nil || row.Store != "run_disk" || row.PausedBy != "alice" {
		t.Fatalf("run_disk row = %+v %v", row, err)
	}
}

// outagePausedRun creates a run kept after an outage (lostAgo before now) whose agent was paused to sign in
// before it, at generation 1, and answers the run as read and the pause's start.
func outagePausedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, st store.PG, lostAgo time.Duration) (types.AgentRun, time.Time) {
	t.Helper()
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, "memory"); err != nil {
		t.Fatal(err)
	}
	gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID)
	if err != nil || gen != 1 {
		t.Fatalf("next pause generation = %d %v", gen, err)
	}
	at := time.Now().UTC().Add(-lostAgo - time.Minute).Truncate(time.Microsecond)
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at, gen); err != nil || !ok {
		t.Fatalf("pause = %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = $2, lost_reason = $3 WHERE id = $1`,
		run.ID, time.Now().UTC().Add(-lostAgo), string(types.LostOutage)); err != nil {
		t.Fatalf("keep the run after an outage: %v", err)
	}
	cur, err := st.GetRun(ctx, run.ID)
	if err != nil || cur.LostAt == nil {
		t.Fatalf("read the kept run = %+v %v", cur, err)
	}
	return cur, at
}

func signInMark(t *testing.T, ctx context.Context, st store.PG, id uuid.UUID) *time.Time {
	t.Helper()
	row, _, err := st.DesktopVMSignIns(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return row.PausedAt
}

// TestPG_EndStoppedDesktopVMAgentPause (#3629): the pause on a run kept after an outage is cleared once,
// and only while the run still carries that outage mark with its agent's stop recorded under it, and the
// row still the pause the sweep read.
func TestPG_EndStoppedDesktopVMAgentPause(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)

	t.Run("a confirmed stop clears the pause once", func(t *testing.T) {
		run, at := outagePausedRun(t, ctx, pool, st, time.Minute)
		if err := st.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); err != nil {
			t.Fatal(err)
		}
		var stopped time.Time
		if err := pool.QueryRow(ctx, `SELECT agent_stopped_at FROM agent_runs WHERE id = $1`, run.ID).Scan(&stopped); err != nil {
			t.Fatal(err)
		}
		by, gotAt, gotStop, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1)
		if err != nil || !ok || by != "alice" || !gotAt.Equal(at) || !gotStop.Equal(stopped) {
			t.Fatalf("end = %q %v %v %v %v, want alice's pause at %v and the stop at %v", by, gotAt, gotStop, ok, err, at, stopped)
		}
		if p := signInMark(t, ctx, st, run.ID); p != nil {
			t.Fatalf("the mark stands at %v after the end", p)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1); err != nil || ok {
			t.Fatalf("a second end = %v %v, want nothing cleared", ok, err)
		}
	})

	t.Run("no recorded stop stays paused however long ago the run was lost", func(t *testing.T) {
		run, at := outagePausedRun(t, ctx, pool, st, 30*24*time.Hour)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at = now() - interval '29 days' WHERE id = $1`, run.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1); err != nil || ok {
			t.Fatalf("end with no stop recorded = %v %v, want nothing cleared", ok, err)
		}
		if p := signInMark(t, ctx, st, run.ID); p == nil || !p.Equal(at) {
			t.Fatalf("mark = %v, want the pause at %v standing", p, at)
		}
	})

	t.Run("a stop under another outage mark does not clear", func(t *testing.T) {
		run, at := outagePausedRun(t, ctx, pool, st, time.Minute)
		older := run.LostAt.Add(-time.Second)
		if err := st.MarkRunAgentStopped(ctx, run.ID, older); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1); err != nil || ok {
			t.Fatalf("end after a stop decided under an older mark = %v %v, want nothing cleared", ok, err)
		}
		if err := st.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, older, at, 1); err != nil || ok {
			t.Fatalf("end carrying an older outage mark = %v %v, want nothing cleared", ok, err)
		}
		if p := signInMark(t, ctx, st, run.ID); p == nil {
			t.Fatal("the mark was cleared")
		}
	})

	t.Run("a revive first leaves the pause", func(t *testing.T) {
		run, at := outagePausedRun(t, ctx, pool, st, time.Minute)
		if err := st.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.MarkRunRevived(ctx, run.ID, types.LostOutage, nil, 0, false); err != nil || !ok {
			t.Fatalf("revive = %v %v", ok, err)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1); err != nil || ok {
			t.Fatalf("end after a revive = %v %v, want nothing cleared", ok, err)
		}
		if p := signInMark(t, ctx, st, run.ID); p == nil {
			t.Fatal("the mark was cleared after a revive")
		}
	})

	t.Run("a newer pause generation survives", func(t *testing.T) {
		run, at := outagePausedRun(t, ctx, pool, st, time.Minute)
		if err := st.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at, 1); err != nil || !ok {
			t.Fatalf("resume = %v %v", ok, err)
		}
		gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID)
		if err != nil || gen != 2 {
			t.Fatalf("next pause generation = %d %v", gen, err)
		}
		if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "bob", at, gen); err != nil || !ok {
			t.Fatalf("a newer pause = %v %v", ok, err)
		}
		if _, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1); err != nil || ok {
			t.Fatalf("end of the older generation = %v %v, want nothing cleared", ok, err)
		}
		if row, _, err := st.DesktopVMSignIns(ctx, run.ID); err != nil || row.PausedBy != "bob" || row.PauseGeneration != 2 {
			t.Fatalf("row = %+v %v, want bob's pause at generation 2", row, err)
		}
	})
}

// TestPG_EndStoppedDesktopVMAgentPause_WaitsForARevive (#3629): a revive that clears the outage mark and the
// stop between the sweep's read and its end, in a transaction still open when the end runs, makes the end
// wait on the run's row and then clear nothing. An end that read the stop apart from its clear would see
// the stop the revive has not yet committed away and clear the pause of an agent about to run again.
func TestPG_EndStoppedDesktopVMAgentPause_WaitsForARevive(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run, at := outagePausedRun(t, ctx, pool, st, time.Minute)
	if err := st.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); err != nil {
		t.Fatal(err)
	}
	reviver, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reviver.Rollback(context.WithoutCancel(ctx)) }()
	var reviverPID int
	if err := reviver.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&reviverPID); err != nil {
		t.Fatal(err)
	}
	if _, err := reviver.Exec(ctx, `UPDATE agent_runs SET lost_at = NULL, lost_reason = '', agent_stopped_at = NULL WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		ok  bool
		err error
	}
	result := make(chan answer, 1)
	go func() {
		_, _, _, ok, err := st.EndStoppedDesktopVMAgentPause(ctx, run.ID, *run.LostAt, at, 1)
		result <- answer{ok, err}
	}()
	for {
		// pg_stat_activity is snapshotted per transaction; drop the snapshot so each probe reads fresh.
		if _, err := reviver.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		var waiting bool
		if err := reviver.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, reviverPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case got := <-result:
			t.Fatalf("the end answered %v %v without waiting for the open revive", got.ok, got.err)
		case <-ctx.Done():
			t.Fatal("the end never waited for the open revive")
		case <-time.After(time.Millisecond):
		}
	}
	if err := reviver.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.ok {
			t.Fatalf("end after the revive committed = %v %v, want nothing cleared", got.ok, got.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if p := signInMark(t, ctx, st, run.ID); p == nil || !p.Equal(at) {
		t.Fatalf("mark = %v, want the pause at %v standing for the revived agent", p, at)
	}
}
