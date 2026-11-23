// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// outageDB is a migrated database of its own: cp_outage and server_outage windows are global, so no test shares them.
func outageDB(t *testing.T) (*pgxpool.Pool, store.PG) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	return pool, store.NewPG(pool)
}

// outageRunner is a claimed runner whose inventory row holds session.
func outageRunner(t *testing.T, st store.PG, session string) types.Runner {
	t.Helper()
	ctx := context.Background()
	rn, err := st.CreateRunner(ctx, newRunner("outage-owner-"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	if session != "" {
		if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
			t.Fatal(err)
		}
	}
	return rn
}

func outageDBNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// outageInsert writes a window directly; ended nil leaves it open.
func outageInsert(t *testing.T, pool *pgxpool.Pool, kind string, runner *uuid.UUID, started time.Time, ended *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO outage_windows (kind, runner_id, started_at, ended_at) VALUES ($1, $2, $3, $4)`,
		kind, runner, started, ended); err != nil {
		t.Fatalf("insert %s window: %v", kind, err)
	}
}

func outageCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outage_windows WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func outageCovers(t *testing.T, st store.PG, runner *uuid.UUID, placement types.Placement, expiry time.Time) (store.OutageWindow, bool) {
	t.Helper()
	w, ok, err := st.OutageWindowCovers(context.Background(), runner, placement, expiry)
	if err != nil {
		t.Fatalf("OutageWindowCovers: %v", err)
	}
	return w, ok
}

// A runner_link window covers only its runner's runs; a server_outage window covers only server runs (no runner, not
// local), so a deleted runner's local run is not covered by it.
func TestOutageWindowCovers_KindsAndScope_PG(t *testing.T) {
	pool, st := outageDB(t)
	expiry := outageDBNow(t, pool)
	a, b := uuid.New(), uuid.New()
	outageInsert(t, pool, store.OutageKindRunnerLink, &a, expiry.Add(-20*time.Minute), nil)
	if w, ok := outageCovers(t, st, &a, types.PlacementLocal, expiry); !ok || w.Kind != store.OutageKindRunnerLink || w.RunnerID == nil || *w.RunnerID != a || w.EndedAt != nil {
		t.Fatalf("runner A's run: covered %v by %+v; want its open runner_link window", ok, w)
	}
	if _, ok := outageCovers(t, st, &b, types.PlacementLocal, expiry); ok {
		t.Fatal("runner A's window covers runner B's run")
	}
	if _, ok := outageCovers(t, st, nil, types.PlacementRemote, expiry); ok {
		t.Fatal("a runner_link window covers a server run")
	}
	outageInsert(t, pool, store.OutageKindServer, nil, expiry.Add(-20*time.Minute), nil)
	if w, ok := outageCovers(t, st, nil, types.PlacementRemote, expiry); !ok || w.Kind != store.OutageKindServer {
		t.Fatalf("server run: covered %v by %+v; want the server_outage window", ok, w)
	}
	if _, ok := outageCovers(t, st, nil, types.PlacementLocal, expiry); ok {
		t.Fatal("a server_outage window covers a deleted runner's local run")
	}
	if _, ok := outageCovers(t, st, &b, types.PlacementLocal, expiry); ok {
		t.Fatal("a server_outage window covers a runner's run")
	}
}

// Every window ends: the heal closes a runner_link window whose runner was deleted or that is open past 25 hours,
// and leaves a younger one for a runner that still exists.
func TestOutageWindows_HealClosesDeletedAndOver25h_PG(t *testing.T) {
	pool, st := outageDB(t)
	now := outageDBNow(t, pool)
	gone := uuid.New()
	old, young := outageRunner(t, st, ""), outageRunner(t, st, "")
	outageInsert(t, pool, store.OutageKindRunnerLink, &gone, now.Add(-time.Hour), nil)
	outageInsert(t, pool, store.OutageKindRunnerLink, &old.ID, now.Add(-26*time.Hour), nil)
	outageInsert(t, pool, store.OutageKindRunnerLink, &young.ID, now.Add(-time.Hour), nil)
	n, err := st.HealRunnerLinkWindows(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("HealRunnerLinkWindows = %d, %v; want 2 closed", n, err)
	}
	if open := outageCount(t, pool, `ended_at IS NULL`); open != 1 {
		t.Fatalf("open windows = %d, want 1", open)
	}
	if open := outageCount(t, pool, `ended_at IS NULL AND runner_id = $1`, young.ID); open != 1 {
		t.Fatal("the heal closed the window of a runner that exists and is under 25 hours")
	}
}

// A cp_outage window is only ever written closed.
func TestOutageWindows_OpenCPOutageRefused_PG(t *testing.T) {
	pool, _ := outageDB(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO outage_windows (kind, started_at) VALUES ('cp_outage', now())`); err == nil {
		t.Fatal("an open cp_outage window was accepted")
	}
}

// The gap is measured on the database's clock: two beats 10 s apart write no window, a beat 45 s after the last
// writes one of about 45 s.
func TestBeatControlPlane_GapOnDatabaseClock_PG(t *testing.T) {
	pool, st := outageDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE cp_heartbeat SET beat_at = now() - interval '10 seconds'`); err != nil {
		t.Fatal(err)
	}
	if w, err := st.BeatControlPlane(ctx, store.ControlPlaneGapOver); err != nil || w != nil {
		t.Fatalf("beat 10 s after the last = %+v, %v; want no window", w, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cp_heartbeat SET beat_at = now() - interval '45 seconds'`); err != nil {
		t.Fatal(err)
	}
	w, err := st.BeatControlPlane(ctx, store.ControlPlaneGapOver)
	if err != nil || w == nil || w.EndedAt == nil {
		t.Fatalf("beat 45 s after the last = %+v, %v; want a closed window", w, err)
	}
	if gap := w.EndedAt.Sub(w.StartedAt); gap < 43*time.Second || gap > 47*time.Second {
		t.Fatalf("window gap = %v, want about 45 s", gap)
	}
	if n := outageCount(t, pool, `kind = 'cp_outage' AND ended_at IS NOT NULL`); n != 1 {
		t.Fatalf("cp_outage windows = %d, want 1", n)
	}
}

// A runner_link window starts at the drop the hub observed (the database's now minus droppedFor), not at the
// session's start in last_seen_at.
func TestRunnerLinkWindow_StartsAtTheDropNotTheSessionStart_PG(t *testing.T) {
	pool, st := outageDB(t)
	ctx := context.Background()
	session := uuid.NewString()
	rn := outageRunner(t, st, session)
	if _, err := pool.Exec(ctx, `UPDATE runners SET last_seen_at = now() - interval '3 days' WHERE id = $1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, session, 120*time.Second)
	if err != nil || !opened {
		t.Fatalf("OpenRunnerLinkWindow = %v, %v; want opened", opened, err)
	}
	var off float64
	if err := pool.QueryRow(ctx, `SELECT abs(EXTRACT(EPOCH FROM started_at - (now() - interval '120 seconds')))::float8
		FROM outage_windows WHERE kind = 'runner_link' AND runner_id = $1`, rn.ID).Scan(&off); err != nil {
		t.Fatal(err)
	}
	if off > 2 {
		t.Fatalf("window starts %.1f s away from now() - 120 s, want within 2 s", off)
	}
}

// At most one window is open per kind and runner: a second open is a no-op.
func TestOutageWindows_OneOpenPerKind_PG(t *testing.T) {
	pool, st := outageDB(t)
	ctx := context.Background()
	session := uuid.NewString()
	rn := outageRunner(t, st, session)
	other := outageRunner(t, st, uuid.NewString())
	if opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, session, 2*time.Minute); err != nil || !opened {
		t.Fatalf("first open = %v, %v; want opened", opened, err)
	}
	if opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, session, time.Minute); err != nil || opened {
		t.Fatalf("second open = %v, %v; want a no-op", opened, err)
	}
	var otherSession string
	if err := pool.QueryRow(ctx, `SELECT session_id::text FROM runner_inventories WHERE runner_id = $1`, other.ID).Scan(&otherSession); err != nil {
		t.Fatal(err)
	}
	if opened, err := st.OpenRunnerLinkWindow(ctx, other.ID, otherSession, time.Minute); err != nil || !opened {
		t.Fatalf("another runner's open = %v, %v; want opened", opened, err)
	}
	for range 2 {
		if err := st.OpenServerOutageWindow(ctx, 21*time.Minute, map[string]any{"server_runs": 2}); err != nil {
			t.Fatal(err)
		}
	}
	if n := outageCount(t, pool, `kind = 'runner_link' AND runner_id = $1`, rn.ID); n != 1 {
		t.Fatalf("runner_link windows for the runner = %d, want 1", n)
	}
	if n := outageCount(t, pool, `kind = 'server_outage'`); n != 1 {
		t.Fatalf("server_outage windows = %d, want 1", n)
	}
}

// Two replicas beating at once write one window per gap: the row lock serializes them, and the second reads the
// first's fresh beat.
func TestBeatControlPlane_ConcurrentBeatsOneWindow_PG(t *testing.T) {
	pool, st := outageDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE cp_heartbeat SET beat_at = now() - interval '45 seconds'`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]*store.OutageWindow, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], errs[i] = st.BeatControlPlane(ctx, store.ControlPlaneGapOver)
		}()
	}
	close(start)
	wg.Wait()
	written := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("beat %d: %v", i, errs[i])
		}
		if got[i] != nil {
			written++
		}
	}
	if written != 1 {
		t.Fatalf("beats that wrote a window = %d, want 1", written)
	}
	if n := outageCount(t, pool, `kind = 'cp_outage'`); n != 1 {
		t.Fatalf("cp_outage windows = %d, want 1", n)
	}
}

// A window covers only an expiry at least 15 min after its start: a 31 s blip at the expiry covers nothing.
func TestOutageWindowCovers_RequiresLead_PG(t *testing.T) {
	pool, st := outageDB(t)
	now := outageDBNow(t, pool)
	expiry := now.Add(-2 * time.Hour)
	blipEnd := expiry.Add(16 * time.Second)
	outageInsert(t, pool, store.OutageKindControlPlane, nil, expiry.Add(-15*time.Second), &blipEnd)
	if w, ok := outageCovers(t, st, nil, types.PlacementRemote, expiry); ok {
		t.Fatalf("a 31 s window at the expiry covers it: %+v", w)
	}
	longEnd := expiry.Add(time.Minute)
	outageInsert(t, pool, store.OutageKindControlPlane, nil, expiry.Add(-16*time.Minute), &longEnd)
	if w, ok := outageCovers(t, st, nil, types.PlacementRemote, expiry); !ok || !w.StartedAt.Equal(expiry.Add(-16*time.Minute)) {
		t.Fatalf("a window that began 16 min before the expiry: covered %v by %+v", ok, w)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM outage_windows`); err != nil {
		t.Fatal(err)
	}
	runner := uuid.New()
	outageInsert(t, pool, store.OutageKindRunnerLink, &runner, now.Add(-20*time.Minute), nil)
	if w, ok := outageCovers(t, st, &runner, types.PlacementLocal, now); !ok || w.Kind != store.OutageKindRunnerLink {
		t.Fatalf("an open runner_link window that began 20 min before: covered %v by %+v", ok, w)
	}
}

// A late timer cannot open a window for a runner with a newer session, whichever takes the inventory row first.
func TestRunnerLinkWindow_LateTimerAfterFreshSessionOpensNothing_PG(t *testing.T) {
	t.Run("the fresh session began first", func(t *testing.T) {
		pool, st := outageDB(t)
		ctx := context.Background()
		s1 := uuid.NewString()
		rn := outageRunner(t, st, s1)
		if err := st.BeginRunnerInventory(ctx, rn.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, s1, 120*time.Second); err != nil || opened {
			t.Fatalf("OpenRunnerLinkWindow for the replaced session = %v, %v; want false", opened, err)
		}
		if n := outageCount(t, pool, `true`); n != 0 {
			t.Fatalf("windows = %d, want 0", n)
		}
	})
	t.Run("the fresh session holds the row while the timer fires", func(t *testing.T) {
		pool, st := outageDB(t)
		ctx := context.Background()
		s1 := uuid.NewString()
		rn := outageRunner(t, st, s1)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // committed below
		var owner string
		if err := tx.QueryRow(ctx, `SELECT owner FROM runner_inventories WHERE runner_id = $1 FOR UPDATE`, rn.ID).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM runner_inventories WHERE runner_id = $1`, rn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runner_inventories(runner_id, session_id, owner) VALUES ($1, $2, $3)`, rn.ID, uuid.New(), owner); err != nil {
			t.Fatal(err)
		}
		var pid int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		type result struct {
			opened bool
			err    error
		}
		done := make(chan result, 1)
		go func() {
			opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, s1, 120*time.Second)
			done <- result{opened, err}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the timer's statement never waited on the inventory row (%d waiting)", waiting)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil || r.opened {
			t.Fatalf("OpenRunnerLinkWindow behind the fresh session = %v, %v; want false", r.opened, r.err)
		}
		if n := outageCount(t, pool, `true`); n != 0 {
			t.Fatalf("windows = %d, want 0", n)
		}
	})
	t.Run("the timer commits first", func(t *testing.T) {
		pool, st := outageDB(t)
		ctx := context.Background()
		s1 := uuid.NewString()
		rn := outageRunner(t, st, s1)
		if opened, err := st.OpenRunnerLinkWindow(ctx, rn.ID, s1, 120*time.Second); err != nil || !opened {
			t.Fatalf("OpenRunnerLinkWindow = %v, %v; want opened", opened, err)
		}
		if err := st.BeginRunnerInventory(ctx, rn.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if err := st.CloseRunnerLinkWindow(ctx, rn.ID); err != nil {
			t.Fatal(err)
		}
		if n := outageCount(t, pool, `kind = 'runner_link' AND runner_id = $1`, rn.ID); n != 1 {
			t.Fatalf("windows = %d, want 1", n)
		}
		if n := outageCount(t, pool, `ended_at IS NULL`); n != 0 {
			t.Fatal("the window is still open after the fresh session's Connected")
		}
	})
}
