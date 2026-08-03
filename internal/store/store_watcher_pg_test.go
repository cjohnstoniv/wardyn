// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for the run-watcher lease (migration 0027). The whole
// point of the lease is a property no fake can show: the claim is a single
// conditional UPDATE ... RETURNING, and its atomicity under a REAL server is
// what makes "exactly one replica adopts an orphaned run" true. Two sweeps
// racing for the same stale lease are exactly the case an in-memory double
// would happily let both win.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset. The sweep is
// table-wide by design (it looks for any orphan), so every assertion filters to
// the run the test created — the shared dev database always holds other rows.
// Run: WARDYN_TEST_PG=postgres://wardyn:wardyn@localhost:55432/wardyn?sslmode=disable go test -race ./internal/store/...
package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// watchedRun persists a run in `state` WITH a sandbox ref — the sweep only ever
// claims runs that actually have something to watch.
func watchedRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state types.RunState) types.AgentRun {
	t.Helper()
	r := newRun(state)
	r.SandboxRef = "ctr-" + r.ID.String()
	return persistRun(t, ctx, pool, r)
}

// sweepClaims runs one sweep as owner and reports whether it claimed runID.
func sweepClaims(t *testing.T, st store.PG, owner string, staleAfter time.Duration, runID uuid.UUID) bool {
	t.Helper()
	runs, err := st.ClaimStaleRunWatchers(context.Background(), owner, staleAfter)
	if err != nil {
		t.Fatalf("sweep as %s: %v", owner, err)
	}
	for i := range runs {
		if runs[i].ID == runID {
			return true
		}
	}
	return false
}

// stopHeartbeating simulates the watching process dying: its heartbeat simply
// stops, so the lease ages past the stale window. Backdating the column is the
// only way to spend that window in a test.
func stopHeartbeating(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, ago time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE agent_runs SET watcher_heartbeat = now() - $2::interval WHERE id=$1`,
		runID, ago.String()); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
}

func leaseOwner(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) string {
	t.Helper()
	var owner string
	if err := pool.QueryRow(context.Background(),
		`SELECT watcher_owner FROM agent_runs WHERE id=$1`, runID).Scan(&owner); err != nil {
		t.Fatalf("read watcher_owner: %v", err)
	}
	return owner
}

// TestPG_RunWatcherLease_TwoProcessAdoption is the story the lease exists for.
// Control plane A adopts a run and keeps its heartbeat fresh, so B leaves it
// alone; A's pod then dies (the heartbeat just stops) and B adopts the run
// WITHOUT A ever having come back — the residual boot-only reconciliation could
// not close, since a pod that never returns never boots. The final leg is the
// mutual exclusion: two replicas sweeping the same stale lease at once must not
// both start watching the same run.
func TestPG_RunWatcherLease_TwoProcessAdoption(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	run := watchedRun(t, ctx, pool, types.RunRunning)
	const stale = 90 * time.Second

	// A adopts it. A fresh row is born with a live lease (DEFAULT now()), so this
	// is the zero-window sweep: "adopt anything nobody is watching".
	if !sweepClaims(t, st, "A", 0, run.ID) {
		t.Fatal("A's sweep did not claim an unwatched run")
	}
	if got := leaseOwner(t, pool, run.ID); got != "A" {
		t.Fatalf("watcher_owner after A's claim = %q, want \"A\"", got)
	}

	// A is alive and heartbeating: B must NOT steal a run that is being watched.
	if err := st.HeartbeatRunWatcher(ctx, run.ID, "A"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if sweepClaims(t, st, "B", stale, run.ID) {
		t.Fatal("B claimed a run whose watcher is still heartbeating")
	}

	// A's pod dies. Nothing clears the lease — its silence is the whole signal.
	stopHeartbeating(t, pool, run.ID, 5*time.Minute)

	// B and C sweep at the same instant. Exactly one may adopt: the claim
	// UPDATE's predicate is re-evaluated against the row version the loser
	// blocked on, and by then the winner's heartbeat is fresh.
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]bool, 2)
	for i, owner := range []string{"B", "C"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runs, err := st.ClaimStaleRunWatchers(context.Background(), owner, stale)
			if err != nil {
				t.Errorf("concurrent sweep as %s: %v", owner, err)
				return
			}
			for j := range runs {
				if runs[j].ID == run.ID {
					got[i] = true
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, ok := range got {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 concurrent sweeps adopted the stale run, want exactly 1 — the claim is the mutual exclusion", winners)
	}
	if owner := leaseOwner(t, pool, run.ID); owner != "B" && owner != "C" {
		t.Fatalf("watcher_owner after the race = %q, want the sweep that won (B or C)", owner)
	}
	// And the adopted run carries what the adopter probes with (ref + exec id),
	// so a claim is enough to re-derive state without a second read.
	stopHeartbeating(t, pool, run.ID, 5*time.Minute)
	runs, err := st.ClaimStaleRunWatchers(ctx, "D", stale)
	if err != nil {
		t.Fatalf("sweep as D: %v", err)
	}
	for i := range runs {
		if runs[i].ID == run.ID && runs[i].SandboxRef != run.SandboxRef {
			t.Errorf("claimed run lost its sandbox ref: %q, want %q", runs[i].SandboxRef, run.SandboxRef)
		}
	}
}

// TestPG_RunWatcherLease_SkipsTerminalAndUndispatched pins the two predicates
// that keep the sweep from doing harm. A terminal run has already been
// finalized — re-adopting it would poll a torn-down sandbox forever. A run with
// no sandbox ref is not abandoned, it is still being PROVISIONED: the row exists
// for the whole pre-dispatch window (grant writes, then a multi-minute image
// build), so claiming one and reading "no sandbox" as "dead" would kill healthy
// runs. Both stay uncaught even with the window wide open.
func TestPG_RunWatcherLease_SkipsTerminalAndUndispatched(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()

	done := watchedRun(t, ctx, pool, types.RunCompleted)
	undispatched := persistRun(t, ctx, pool, newRun(types.RunPending)) // no sandbox ref
	stopHeartbeating(t, pool, done.ID, 5*time.Minute)
	stopHeartbeating(t, pool, undispatched.ID, 5*time.Minute)

	if sweepClaims(t, st, "A", 0, done.ID) {
		t.Error("the sweep claimed a COMPLETED run; a finalized run must never be re-adopted")
	}
	if sweepClaims(t, st, "A", 0, undispatched.ID) {
		t.Error("the sweep claimed a run with no sandbox ref; that run is still being provisioned, not orphaned")
	}
}

// TestPG_RunWatcherLease_LeavesUpdatedAtAlone: updated_at is the idle reaper's
// activity signal (UpdateRunStateIfIdle guards on it). A lease write is not run
// activity — if the claim or the 30s heartbeat bumped updated_at, every watched
// run would look forever-active and idle auto-stop would silently never fire.
func TestPG_RunWatcherLease_LeavesUpdatedAtAlone(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	run := watchedRun(t, ctx, pool, types.RunRunning)

	before, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !sweepClaims(t, st, "A", 0, run.ID) {
		t.Fatal("sweep did not claim the run")
	}
	if err := st.HeartbeatRunWatcher(ctx, run.ID, "A"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	after, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the watcher lease moved updated_at (%s -> %s); the idle reaper reads that column, so a 30s heartbeat would exempt every watched run from auto-stop",
			before.UpdatedAt, after.UpdatedAt)
	}
}
