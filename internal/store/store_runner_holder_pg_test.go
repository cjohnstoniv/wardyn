// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
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
