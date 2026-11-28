// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holderFixture is a claimed runner on an isolated database.
func holderFixture(t *testing.T) (*pgxpool.Pool, store.PG, types.Runner) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	rn, err := st.CreateRunner(ctx, newRunner("holder-owner"))
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	return pool, st, rn
}

func holderSession(t *testing.T, st store.PG, runnerID uuid.UUID) uuid.UUID {
	t.Helper()
	s := uuid.New()
	if err := st.BeginRunnerInventory(t.Context(), runnerID, s.String()); err != nil {
		t.Fatal(err)
	}
	return s
}

func holderRow(t *testing.T, st store.PG, runnerID uuid.UUID) (store.PeerRouteRow, bool) {
	t.Helper()
	r, ok, err := st.RunnerSessionHolder(t.Context(), runnerID)
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

// R4: a stale admission never claims the holder row.
func TestClaimHolderOnlyForCurrentInventorySession_PG(t *testing.T) {
	_, st, rn := holderFixture(t)
	ctx := t.Context()
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, uuid.New(), "a", "https://a"); err != nil || ok {
		t.Fatalf("claim with no inventory = %v, %v; want false", ok, err)
	}
	s1 := holderSession(t, st, rn.ID)
	s2 := holderSession(t, st, rn.ID)
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s1, "a", "https://a"); err != nil || ok {
		t.Fatalf("claim for the replaced session = %v, %v; want false", ok, err)
	}
	if _, ok := holderRow(t, st, rn.ID); ok {
		t.Fatal("a stale admission wrote the holder row")
	}
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s2, "b", "https://b"); err != nil || !ok {
		t.Fatalf("claim for the current session = %v, %v; want true", ok, err)
	}
	if r, ok := holderRow(t, st, rn.ID); !ok || r.SessionID != s2 || r.ReplicaID != "b" || r.PeerURL != "https://b" {
		t.Fatalf("holder row = %+v, %v; want session %s on b", r, ok, s2)
	}
}

// R4: a claim held open while a newer inventory begins never overwrites the newer row.
func TestClaimHolderSerializesWithInventory_PG(t *testing.T) {
	pool, st, rn := holderFixture(t)
	ctx := t.Context()
	s1 := holderSession(t, st, rn.ID)
	// The S1 claim's transaction, held open after its FOR SHARE read.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed or rolled back below
	var cur uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1 FOR SHARE`, rn.ID).Scan(&cur); err != nil || cur != s1 {
		t.Fatalf("held read = %s, %v", cur, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runner_session_holders(runner_id, session_id, replica_id, peer_url, seen_at) VALUES($1,$2,'a','https://a',now()) ON CONFLICT (runner_id) DO UPDATE SET session_id=EXCLUDED.session_id, replica_id=EXCLUDED.replica_id, peer_url=EXCLUDED.peer_url, seen_at=now()`, rn.ID, s1); err != nil {
		t.Fatal(err)
	}
	s2 := uuid.New()
	began := make(chan error, 1)
	go func() { began <- st.BeginRunnerInventory(ctx, rn.ID, s2.String()) }()
	select {
	case err := <-began:
		t.Fatalf("the newer inventory did not wait for the open claim: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-began; err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s2, "b", "https://b"); err != nil || !ok {
		t.Fatalf("S2 claim = %v, %v", ok, err)
	}
	// A claim for S1 that starts after S2's inventory writes nothing.
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s1, "a", "https://a"); err != nil || ok {
		t.Fatalf("late S1 claim = %v, %v; want false", ok, err)
	}
	if r, ok := holderRow(t, st, rn.ID); !ok || r.SessionID != s2 || r.ReplicaID != "b" {
		t.Fatalf("holder row = %+v, %v; want S2 on b", r, ok)
	}
}

// R4b: a session's end drops only its own row; a touch refreshes only this replica's held sessions.
func TestHolderDropAndTouchNameTheirSession_PG(t *testing.T) {
	pool, st, rn := holderFixture(t)
	ctx := t.Context()
	s1 := holderSession(t, st, rn.ID)
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s1, "a", "https://a"); err != nil || !ok {
		t.Fatalf("A claim = %v, %v", ok, err)
	}
	s2 := holderSession(t, st, rn.ID)
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s2, "b", "https://b"); err != nil || !ok {
		t.Fatalf("B claim = %v, %v", ok, err)
	}
	if err := st.DropRunnerSessionHolder(ctx, rn.ID, s1); err != nil {
		t.Fatal(err)
	}
	if r, ok := holderRow(t, st, rn.ID); !ok || r.SessionID != s2 {
		t.Fatalf("A's drop for S1 removed B's row: %+v, %v", r, ok)
	}
	if _, err := pool.Exec(ctx, `UPDATE runner_session_holders SET seen_at = now() - interval '20 seconds' WHERE runner_id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.TouchRunnerSessionHolders(ctx, "a", []uuid.UUID{s1, s2})
	if err != nil || len(got) != 0 {
		t.Fatalf("A's touch = %v, %v; want nothing touched", got, err)
	}
	var old bool
	if err := pool.QueryRow(ctx, `SELECT now() - seen_at > interval '15 seconds' FROM runner_session_holders WHERE runner_id=$1`, rn.ID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if !old {
		t.Fatal("A's touch refreshed B's row")
	}
	got, err = st.TouchRunnerSessionHolders(ctx, "b", []uuid.UUID{s2})
	if err != nil || len(got) != 1 || got[0] != s2 {
		t.Fatalf("B's touch = %v, %v; want [%s]", got, err, s2)
	}
	if err := st.DropRunnerSessionHolder(ctx, rn.ID, s2); err != nil {
		t.Fatal(err)
	}
	if _, ok := holderRow(t, st, rn.ID); ok {
		t.Fatal("B's drop for S2 left its row")
	}
}

// R5: a holder row older than 30 seconds is never routed to, nor listed.
func TestPeerRouteIgnoresStaleHolder_PG(t *testing.T) {
	pool, st, rn := holderFixture(t)
	ctx := t.Context()
	s := holderSession(t, st, rn.ID)
	if ok, err := st.ClaimRunnerSessionHolder(ctx, rn.ID, s, "a", "https://a"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	listed := func() bool {
		rows, err := st.RunnerSessionHolders(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.RunnerID == rn.ID {
				return true
			}
		}
		return false
	}
	if _, ok := holderRow(t, st, rn.ID); !ok || !listed() {
		t.Fatal("a fresh row is not routed to")
	}
	if _, err := pool.Exec(ctx, `UPDATE runner_session_holders SET seen_at = now() - interval '31 seconds' WHERE runner_id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if r, ok := holderRow(t, st, rn.ID); ok {
		t.Fatalf("a 31 s old row is routed to: %+v", r)
	}
	if listed() {
		t.Fatal("a 31 s old row is listed")
	}
}

// R23 (store): a run's launcher is LauncherFresh only for a lease within 90 s whose process has a replica row within
// 30 s, LauncherWait for a stale lease or a stale or missing replica row, and LauncherNone for a missing, terminal or
// lost run.
func TestPeerLauncherFromFreshLeaseOnly_PG(t *testing.T) {
	pool, st, _ := holderFixture(t)
	ctx := context.Background()
	read := func(id uuid.UUID) store.RunOutputLauncherRow {
		t.Helper()
		r, err := st.RunOutputLauncher(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	set := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if r := read(uuid.New()); r.State != store.LauncherNone {
		t.Fatalf("a missing run: %+v, want LauncherNone", r)
	}
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	set(`UPDATE agent_runs SET watcher_owner='host/p1', watcher_heartbeat=now() WHERE id=$1`, run.ID)
	if r := read(run.ID); r.State != store.LauncherWait || r.WatcherOwner != "host/p1" || !r.LeaseFresh {
		t.Fatalf("a fresh lease with no replica row: %+v, want LauncherWait with the owner", r)
	}
	if err := st.TouchRunnerPeerReplica(ctx, "host/p1", "replica-1", "https://10.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
	if r := read(run.ID); r.State != store.LauncherFresh || r.ReplicaID != "replica-1" || r.PeerURL != "https://10.0.0.1:8443" || r.WatcherOwner != "host/p1" {
		t.Fatalf("a fresh pair: %+v, want LauncherFresh at replica-1", r)
	}
	set(`UPDATE runner_peer_replicas SET seen_at = now() - interval '31 seconds' WHERE lease_owner='host/p1'`)
	if r := read(run.ID); r.State != store.LauncherWait || r.ReplicaID != "" {
		t.Fatalf("a replica row older than 30 s: %+v, want LauncherWait", r)
	}
	set(`UPDATE runner_peer_replicas SET seen_at = now() WHERE lease_owner='host/p1'`)
	set(`UPDATE agent_runs SET watcher_heartbeat = now() - interval '91 seconds' WHERE id=$1`, run.ID)
	if r := read(run.ID); r.State != store.LauncherWait || r.LeaseFresh {
		t.Fatalf("a lease older than 90 s: %+v, want LauncherWait and a stale lease", r)
	}
	set(`UPDATE agent_runs SET watcher_heartbeat = now(), lost_at = now() WHERE id=$1`, run.ID)
	if r := read(run.ID); r.State != store.LauncherNone {
		t.Fatalf("a lost run: %+v, want LauncherNone", r)
	}
	set(`UPDATE agent_runs SET lost_at = NULL, state = $2, ended_at = now() WHERE id=$1`, run.ID, string(types.RunCompleted))
	if r := read(run.ID); r.State != store.LauncherNone {
		t.Fatalf("a terminal run: %+v, want LauncherNone", r)
	}
}

// chunkReplicas is runID's chunks' replica column, in seq order.
func chunkReplicas(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT replica FROM run_output_chunks WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// R23f: a live adoption's recovery tail replaces the launcher's chunks, and a later adoption supersedes it.
func TestPeerAdoptReplacesRunOutputChunks_PG(t *testing.T) {
	pool, st, _ := holderFixture(t)
	ctx := t.Context()
	const keep = 1 << 20
	newRunID := func() uuid.UUID {
		t.Helper()
		r, err := st.CreateRun(ctx, newRun(types.RunRunning))
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	plain := func(run uuid.UUID, replica, b string) bool {
		t.Helper()
		wrote, err := st.AppendRunOutputChunk(ctx, run, replica, []byte(b), keep)
		if err != nil {
			t.Fatal(err)
		}
		return wrote
	}
	recovered := func(run uuid.UUID, replica, b string, first bool) bool {
		t.Helper()
		wrote, err := st.AppendRecoveredRunOutputChunk(ctx, run, replica, []byte(b), keep, first)
		if err != nil {
			t.Fatal(err)
		}
		return wrote
	}
	read := func(run uuid.UUID) store.RunOutputChunks {
		t.Helper()
		ch, err := st.ReadRunOutputChunks(ctx, run, keep)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	other := func(run uuid.UUID, mine string) bool {
		t.Helper()
		ok, err := st.RecoveredRunOutputChunks(ctx, run, mine)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	run := newRunID()
	for _, b := range []string{"a1", "a2", "a3"} {
		if !plain(run, "a", b) {
			t.Fatalf("the launcher's append %q wrote nothing", b)
		}
	}
	if !recovered(run, "b", "b1", true) {
		t.Fatal("the recovery's first append wrote nothing")
	}
	if ch := read(run); string(ch.Bytes) != "b1" || ch.Truncated || !ch.Recovered {
		t.Fatalf("after the first recovered append: %q truncated=%v recovered=%v, want only b1, not truncated, recovered", ch.Bytes, ch.Truncated, ch.Recovered)
	}
	if plain(run, "a", "a4") {
		t.Fatal("the launcher's append wrote over a recovery's chunks")
	}
	if !recovered(run, "b", "b2", false) {
		t.Fatal("the recovery's next append wrote nothing")
	}
	if ch, reps := read(run), chunkReplicas(t, pool, run); string(ch.Bytes) != "b1b2" || len(reps) != 2 || reps[0] != "recovered:b" || reps[1] != "recovered:b" {
		t.Fatalf("the recovery's chunks: %q on %v, want b1b2 on recovered:b", ch.Bytes, reps)
	}
	if !other(run, "") || other(run, "b") {
		t.Fatal("RecoveredRunOutputChunks: want true for any recovery and false for b's own")
	}
	onlyLauncher := newRunID()
	plain(onlyLauncher, "a", "a1")
	if other(onlyLauncher, "") || read(onlyLauncher).Recovered {
		t.Fatal("a run with only the launcher's chunks reads as recovered")
	}

	// A later adoption's first append supersedes b, whose next append then writes nothing.
	if !recovered(run, "c", "c1", true) {
		t.Fatal("the later adoption's first append wrote nothing")
	}
	if recovered(run, "b", "b3", false) {
		t.Fatal("the superseded recovery's append wrote")
	}
	if ch := read(run); string(ch.Bytes) != "c1" {
		t.Fatalf("after the superseded append: %q, want c1", ch.Bytes)
	}
	if !other(run, "b") || other(run, "c") {
		t.Fatal("RecoveredRunOutputChunks after the supersession: want true for b and false for c")
	}
	if !recovered(run, "c", "c2", false) || string(read(run).Bytes) != "c1c2" {
		t.Fatal("the later recovery's next append did not write")
	}

	// After the final row every append writes nothing.
	if err := st.SaveFinalRunOutput(ctx, store.RunOutput{RunID: run, Output: []byte("final"), Source: "stdout"}); err != nil {
		t.Fatal(err)
	}
	if plain(run, "a", "x") || recovered(run, "c", "x", false) || recovered(run, "d", "x", true) {
		t.Fatal("an append after the final row wrote")
	}

	// An erased run answers ErrRunOutputErased to every method.
	erased := newRunID()
	recovered(erased, "b", "b1", true)
	if err := st.EraseRunOutputs(ctx, []uuid.UUID{erased}); err != nil {
		t.Fatal(err)
	}
	_, e1 := st.AppendRunOutputChunk(ctx, erased, "a", []byte("x"), keep)
	_, e2 := st.AppendRecoveredRunOutputChunk(ctx, erased, "b", []byte("x"), keep, false)
	_, e3 := st.AppendRecoveredRunOutputChunk(ctx, erased, "c", []byte("x"), keep, true)
	_, e4 := st.RecoveredRunOutputChunks(ctx, erased, "")
	_, e5 := st.ReadRunOutputChunks(ctx, erased, keep)
	_, e6 := st.SaveChunkedRunOutput(ctx, erased, "", keep)
	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if !errors.Is(err, store.ErrRunOutputErased) {
			t.Fatalf("method %d on an erased run: %v, want ErrRunOutputErased", i+1, err)
		}
	}
}

// A launcher flush of a whole retained tail, after a recovery's short write, evicts none of the recovery's bytes:
// the launcher's append answers false and writes and trims nothing.
func TestPeerLauncherAppendKeepsRecoveredChunks_PG(t *testing.T) {
	pool, st, _ := holderFixture(t)
	ctx := t.Context()
	const keep = 64
	r, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := st.AppendRecoveredRunOutputChunk(ctx, r.ID, "b", []byte("r"), keep, true); err != nil || !wrote {
		t.Fatalf("the recovery's short write: %v %v", wrote, err)
	}
	wrote, err := st.AppendRunOutputChunk(ctx, r.ID, "a", bytes.Repeat([]byte("x"), keep), keep)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("the launcher's full-tail append wrote after a recovery owned the chunks")
	}
	ch, err := st.ReadRunOutputChunks(ctx, r.ID, keep)
	if err != nil {
		t.Fatal(err)
	}
	if reps := chunkReplicas(t, pool, r.ID); string(ch.Bytes) != "r" || !ch.Recovered || len(reps) != 1 || reps[0] != "recovered:b" {
		t.Fatalf("after the launcher's append: %q recovered=%v on %v, want the recovery's r on recovered:b", ch.Bytes, ch.Recovered, reps)
	}
}
