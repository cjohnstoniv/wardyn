// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// guardedWriteBehindAnUnbind is TestPG_IdentityGuard_RefusesAWriteQueuedBehindAnUnbind's race for any
// guarded writer: write runs under sub's identity guard while an unbind holds sub's identity row, queues
// behind it, and is answered once the unbind has released the principal and written its cutoff an hour in
// the past (as a clock that stepped back would). It returns what write answered.
func guardedWriteBehindAnUnbind(t *testing.T, st store.PG, sub string, write func(ctx context.Context) error) error {
	t.Helper()
	pool := pgPool(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: sub, Issuer: "https://login.example/" + uuid.NewString() + "/v2.0", TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM principal_identities WHERE id = $1 FOR UPDATE`, row.ID); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- write(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: -1}))
	}()
	// Wait until the guarded write is blocked on the row lock, so its guard began before the cutoff.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := tx.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		var waiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))
			AND query ILIKE '%principal_identities%FOR SHARE%')`, blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the guarded write finished (%v) without queuing behind the identity row", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the guarded write never queued behind the row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE principal_identities SET principal = NULL WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - interval '1 hour')
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, sub); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// TestPG_IdentityGuard_RefusesAQueuedRunBehindAnUnbind: a guarded CreateQueuedRun that queued behind an
// unbind is refused by the cutoff the unbind wrote, ordered by sequence, and leaves no run or queue row.
func TestPG_IdentityGuard_RefusesAQueuedRunBehindAnUnbind(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	p := queuePool(t, st)
	sub := "sub-" + uuid.NewString()
	now := time.Now().UTC()
	err := guardedWriteBehindAnUnbind(t, st, sub, func(ctx context.Context) error {
		_, err := st.CreateQueuedRun(ctx, poolRun(p, types.RunQueued, sub), store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		return err
	})
	if !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("the queued launch behind the unbind = %v, want ErrIdentityDeactivated", err)
	}
	pool := pgPool(t, st)
	if n := countRows(t, pool, `SELECT count(*) FROM agent_runs WHERE created_by = $1`, sub) + countRows(t, pool, `SELECT count(*) FROM run_queue WHERE owner = $1`, sub); n != 0 {
		t.Fatalf("%d run or queue row(s) landed under the released sub, want 0", n)
	}
}

// TestPG_IdentityGuard_RefusesARunWithAnIntentBehindAnUnbind: the same for a CreateRun that carries a
// launch intent, which writes through its own transaction.
func TestPG_IdentityGuard_RefusesARunWithAnIntentBehindAnUnbind(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	sub := "sub-" + uuid.NewString()
	err := guardedWriteBehindAnUnbind(t, st, sub, func(ctx context.Context) error {
		_, err := st.CreateRun(ctx, intentRun(sub, `{"shape":"task"}`))
		return err
	})
	if !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("the launch with an intent behind the unbind = %v, want ErrIdentityDeactivated", err)
	}
	pool := pgPool(t, st)
	if n := countRows(t, pool, `SELECT count(*) FROM agent_runs WHERE created_by = $1`, sub) + countRows(t, pool, `SELECT count(*) FROM run_launch_intents WHERE created_by = $1`, sub); n != 0 {
		t.Fatalf("%d run or intent row(s) landed under the released sub, want 0", n)
	}
}

// TestPG_IdentityGuard_RefusesARunWithAnIntentForADeactivatedOwner: a CreateRun that carries a launch
// intent honours the identity guard: a deactivated owner's is refused and nothing is written; without the
// guard on the context the same write goes through.
func TestPG_IdentityGuard_RefusesARunWithAnIntentForADeactivatedOwner(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	pool := pgPool(t, st)
	ctx := context.Background()
	sub := "sub-" + uuid.NewString()
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: sub, Issuer: "https://login.example/" + uuid.NewString() + "/v2.0", TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, sub); err != nil {
		t.Fatal(err)
	}
	g := store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: -1})
	if _, err := st.CreateRun(g, intentRun(sub, `{"shape":"task"}`)); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("a deactivated owner's launch with an intent = %v, want ErrIdentityDeactivated", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM agent_runs WHERE created_by = $1`, sub); n != 0 {
		t.Fatalf("%d run row(s) landed for the deactivated owner, want 0", n)
	}
	if _, err := st.CreateRun(ctx, intentRun(sub, `{"shape":"task"}`)); err != nil {
		t.Fatalf("control: the same launch with no guard on the context = %v, want it written", err)
	}
}
