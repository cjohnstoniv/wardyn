// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for the sweep families: two replicas share them, a family's lock is never released while a
// unit of its term runs, an older replica's exclusive sweeper lock and the family leaders exclude each other, and a
// family freed by a lost leader is taken at the next round. Each replica is a leader on its own pool.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// famLeader is a leader on its own pool that tries families every retry and checks its connection every 50 ms.
func famLeader(t *testing.T, name string, retry time.Duration) *SweeperLeader {
	t.Helper()
	return famLeaderOn(pgPool(t), name, retry)
}

// famLeaderOn is famLeader on pool.
func famLeaderOn(pool *pgxpool.Pool, name string, retry time.Duration) *SweeperLeader {
	l := NewSweeperLeader(pool, name)
	l.retry, l.monitor, l.poll = retry, 50*time.Millisecond, 10*time.Millisecond
	return l
}

// runLeader runs l until the returned stop, which waits for Run to return; the test's cleanup stops it too.
func runLeader(t *testing.T, l *SweeperLeader) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func leads(l *SweeperLeader, family string) bool {
	_, ok := l.Family(family).Held()
	return ok
}

func ledFamilies(l *SweeperLeader) []string {
	var out []string
	for _, name := range SweeperFamilies {
		if leads(l, name) {
			out = append(out, name)
		}
	}
	return out
}

// familyFree reports whether a session of pool can take family's lock now; it gives the lock back at once.
func familyFree(t *testing.T, pool *pgxpool.Pool, family string) bool {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	obj := NewLockKey(SweeperFamilyLockClass, family).Obj
	var got bool
	if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, SweeperFamilyLockClass, obj).Scan(&got); err != nil {
		t.Fatalf("try family lock: %v", err)
	}
	if got {
		if _, err := c.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, SweeperFamilyLockClass, obj); err != nil {
			t.Fatalf("give the family lock back: %v", err)
		}
	}
	return got
}

// legacyFree reports whether a session of pool can take the single sweeper lock exclusively now, as an older
// replica does; it gives the lock back at once.
func legacyFree(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	var got bool
	if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, SweeperLeaderLockKey).Scan(&got); err != nil {
		t.Fatalf("try the sweeper lock: %v", err)
	}
	if got {
		if _, err := c.Exec(ctx, `SELECT pg_advisory_unlock($1)`, SweeperLeaderLockKey); err != nil {
			t.Fatalf("give the sweeper lock back: %v", err)
		}
	}
	return got
}

func familyEpochs(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT family, epoch FROM sweeper_families`)
	if err != nil {
		t.Fatalf("read family epochs: %v", err)
	}
	out := map[string]int64{}
	for rows.Next() {
		var f string
		var e int64
		if err := rows.Scan(&f, &e); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[f] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read family epochs: %v", err)
	}
	return out
}

func terminate(t *testing.T, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}
}

// TestFamiliesSpreadAcrossTwoLeaders_PG: two replicas started together share the four families, no family is ever
// led by both, and each family's epoch moved on exactly once, for its one acquisition.
func TestFamiliesSpreadAcrossTwoLeaders_PG(t *testing.T) {
	a, b := famLeader(t, "replica-a", 100*time.Millisecond), famLeader(t, "replica-b", 100*time.Millisecond)
	before := familyEpochs(t, a.pool)
	runLeader(t, a)
	runLeader(t, b)
	waitFor(t, "all four families led", 5*time.Second, func() bool {
		return len(ledFamilies(a))+len(ledFamilies(b)) == len(SweeperFamilies)
	})
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, f := range SweeperFamilies {
			if leads(a, f) && leads(b, f) {
				t.Fatalf("both replicas lead %q", f)
			}
		}
	}
	if len(ledFamilies(a)) == 0 || len(ledFamilies(b)) == 0 {
		t.Fatalf("a leads %v, b leads %v; want the families shared", ledFamilies(a), ledFamilies(b))
	}
	after := familyEpochs(t, a.pool)
	for _, f := range SweeperFamilies {
		if after[f] != before[f]+1 {
			t.Errorf("family %q epoch %d -> %d; want one bump for one acquisition", f, before[f], after[f])
		}
	}
}

// TestFamilyLossJoinsBeforeUnlock_PG: a moved epoch, and then a failed monitor query, each end the family's term. The
// unit returns 300 ms after its cancel, and the family's lock, tried from another session, stays held until it has
// returned; the election connection is not closed while it runs.
func TestFamilyLossJoinsBeforeUnlock_PG(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause func(t *testing.T, probe *pgxpool.Pool)
		after func(t *testing.T, probe *pgxpool.Pool)
	}{
		{"moved epoch", func(t *testing.T, probe *pgxpool.Pool) {
			if _, err := probe.Exec(context.Background(), `UPDATE sweeper_families SET epoch = epoch + 1 WHERE family = 'runs'`); err != nil {
				t.Fatalf("move the epoch: %v", err)
			}
		}, func(*testing.T, *pgxpool.Pool) {}},
		{"failed monitor query", func(t *testing.T, probe *pgxpool.Pool) {
			if _, err := probe.Exec(context.Background(), `ALTER TABLE sweeper_families RENAME TO sweeper_families_away`); err != nil {
				t.Fatalf("hide the families table: %v", err)
			}
		}, func(t *testing.T, probe *pgxpool.Pool) {
			if _, err := probe.Exec(context.Background(), `ALTER TABLE sweeper_families_away RENAME TO sweeper_families`); err != nil {
				t.Fatalf("restore the families table: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A database of its own: the renamed table, should the binary die before it is restored, is no other
			// test's.
			probe := pgPoolIsolated(t)
			restored := false
			defer func() {
				if !restored {
					tc.after(t, probe)
				}
			}()
			l := famLeaderOn(probe, "replica-a", time.Hour) // one round: only the boot's take
			var started, returned atomic.Bool
			stop := runLeader(t, l)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go l.Family(SweeperFamilyRuns).Go(ctx, func(c context.Context) {
				started.Store(true)
				<-c.Done()
				time.Sleep(300 * time.Millisecond)
				returned.Store(true)
			})
			waitFor(t, "the runs unit to start", 5*time.Second, started.Load)
			pid := electionPID(t, l)
			tc.cause(t, probe)
			waitFor(t, "the runs term to end", 5*time.Second, func() bool { return !leads(l, SweeperFamilyRuns) })
			for !returned.Load() {
				if familyFree(t, probe, SweeperFamilyRuns) && !returned.Load() {
					t.Fatal("the family's lock was free while its unit still ran")
				}
				var alive bool
				if err := probe.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
					t.Fatalf("read pg_stat_activity: %v", err)
				}
				if !alive && !returned.Load() {
					t.Fatal("the election connection was closed while a unit still ran")
				}
				time.Sleep(10 * time.Millisecond)
			}
			tc.after(t, probe)
			restored = true
			waitFor(t, "the family's lock to be free once its unit returned", 5*time.Second, func() bool {
				return familyFree(t, probe, SweeperFamilyRuns)
			})
			stop()
		})
	}
}

// TestLegacyLeaderBlocksFamilies_PG: while an older replica holds the single sweeper lock exclusively a replica of
// this version leads no family, and once it lets go the replica leads, holding the key in shared mode, so an older
// replica's exclusive try fails and its probe sees the key held.
func TestLegacyLeaderBlocksFamilies_PG(t *testing.T) {
	older := poolOfOne(t)
	_, release, ok, err := TryAdvisoryLockDedicated(context.Background(), older, SweeperLeaderLockKey)
	if err != nil || !ok {
		t.Fatalf("the older replica took the sweeper lock = %v, %v", ok, err)
	}
	l := famLeader(t, "replica-a", 50*time.Millisecond)
	runLeader(t, l)
	time.Sleep(400 * time.Millisecond) // several rounds
	if got := ledFamilies(l); len(got) != 0 {
		release()
		t.Fatalf("leads %v while an older replica holds the sweeper lock", got)
	}
	release()
	waitFor(t, "the replica to lead once the older one let go", 5*time.Second, func() bool { return len(ledFamilies(l)) > 0 })
	probe := pgPool(t)
	if legacyFree(t, probe) {
		t.Fatal("an older replica's exclusive try succeeded while a family leader holds the key shared")
	}
	if held, err := AdvisoryLockHeld(context.Background(), probe, SweeperLeaderLockKey); err != nil || !held {
		t.Fatalf("AdvisoryLockHeld = %v, %v; want held", held, err)
	}
}

// TestLegacyKeyHeldSharedByEveryFamilyLeader_PG: n1 leads runs and n2 the rest. After n1 resigns, and again after
// n1's election session is ended, an older replica's exclusive try still fails and its probe sees the key held,
// because n2 still leads.
func TestLegacyKeyHeldSharedByEveryFamilyLeader_PG(t *testing.T) {
	n1 := famLeader(t, "replica-n1", time.Hour) // takes runs at boot, then nothing more
	runLeader(t, n1)
	waitFor(t, "n1 to lead runs", 5*time.Second, func() bool { return leads(n1, SweeperFamilyRuns) })
	n2 := famLeader(t, "replica-n2", 50*time.Millisecond)
	runLeader(t, n2)
	waitFor(t, "n2 to lead the rest", 5*time.Second, func() bool { return len(ledFamilies(n2)) == len(SweeperFamilies)-1 })
	probe := pgPool(t)
	blocked := func(step string) {
		t.Helper()
		if legacyFree(t, probe) {
			t.Fatalf("%s: an older replica's exclusive try succeeded", step)
		}
		if held, err := AdvisoryLockHeld(context.Background(), probe, SweeperLeaderLockKey); err != nil || !held {
			t.Fatalf("%s: AdvisoryLockHeld = %v, %v; want held", step, held, err)
		}
	}
	blocked("both lead")
	n1.Family(SweeperFamilyRuns).Resign()
	if leads(n1, SweeperFamilyRuns) {
		t.Fatal("n1 still leads runs after resigning it")
	}
	blocked("n1 resigned")
	terminate(t, probe, electionPID(t, n1))
	blocked("n1's session ended")
}

// TestSweeperLeaseIsRunsFamily_PG: the leader's Join and Current, which the run pause's fence uses, are the runs
// family's.
func TestSweeperLeaseIsRunsFamily_PG(t *testing.T) {
	l := famLeader(t, "replica-a", time.Hour)
	runLeader(t, l)
	waitFor(t, "runs to be led", 5*time.Second, func() bool { return leads(l, SweeperFamilyRuns) })
	_, epoch, end, ok := l.Join()
	if !ok {
		t.Fatal("the leader's Join answered false while it leads runs")
	}
	defer end()
	_, runsEpoch, runsEnd, ok := l.Family(SweeperFamilyRuns).Join()
	if !ok {
		t.Fatal("the runs family's Join answered false")
	}
	defer runsEnd()
	if epoch != runsEpoch {
		t.Fatalf("Join epoch %d, runs family epoch %d; want the same", epoch, runsEpoch)
	}
	if cur, err := l.Current(context.Background(), epoch); err != nil || !cur {
		t.Fatalf("Current(%d) = %v, %v; want true", epoch, cur, err)
	}
	if info, err := l.Info(context.Background()); err != nil || info.Epoch != epoch || !info.Self {
		t.Fatalf("Info = %+v, %v; want Self at epoch %d", info, err, epoch)
	}
}

// TestFamilyResignStuckSweepKeepsLock_PG: a unit that ignores its context keeps its family's lock past Resign, the
// family is reported stalled, the unit is entered once, a second Resign changes nothing at once, and the lock is
// free within a round of the unit returning.
func TestFamilyResignStuckSweepKeepsLock_PG(t *testing.T) {
	l := famLeader(t, "replica-a", 100*time.Millisecond)
	l.resignJoin = 200 * time.Millisecond
	probe := pgPool(t)
	runLeader(t, l)
	var entered, returned atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Family(SweeperFamilyRuns).Go(ctx, func(context.Context) {
		entered.Add(1)
		time.Sleep(2 * time.Second) // ignores its context
		returned.Add(1)
	})
	waitFor(t, "the unit to start", 5*time.Second, func() bool { return entered.Load() == 1 })
	l.Family(SweeperFamilyRuns).Resign()
	if familyFree(t, probe, SweeperFamilyRuns) {
		t.Fatal("the family's lock was released while its unit still ran")
	}
	if got := l.Stalled(); !slices.Equal(got, []string{SweeperFamilyRuns}) {
		t.Fatalf("Stalled() = %v; want [runs]", got)
	}
	start := time.Now()
	l.Family(SweeperFamilyRuns).Resign()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("a second Resign of a stalled family took %s; want at once", d)
	}
	if got := l.Stalled(); !slices.Equal(got, []string{SweeperFamilyRuns}) || familyFree(t, probe, SweeperFamilyRuns) {
		t.Fatalf("after a second Resign Stalled() = %v; want the family still stalled and its lock kept", got)
	}
	waitFor(t, "the unit to return", 5*time.Second, func() bool { return returned.Load() == 1 })
	waitFor(t, "the lock to be free within a round", 2*l.retry+time.Second, func() bool {
		return familyFree(t, probe, SweeperFamilyRuns)
	})
	if n := entered.Load(); n != 1 {
		t.Fatalf("the unit was entered %d times; want once", n)
	}
	if got := l.Stalled(); len(got) != 0 {
		t.Fatalf("Stalled() = %v once the unit returned; want none", got)
	}
}

// TestFamilyRetakeRefusedWhileSweepAlive_PG: while a resigned family's stuck unit lives, this process neither re-takes
// the family nor lets a per-tick Join user in, and fn is not entered again. Once the unit returns and the cooldown
// passes the family is taken again and fn runs a second time, never beside the first.
func TestFamilyRetakeRefusedWhileSweepAlive_PG(t *testing.T) {
	l := famLeader(t, "replica-a", 100*time.Millisecond)
	l.resignJoin, l.cooldown = 200*time.Millisecond, 300*time.Millisecond
	runLeader(t, l)
	var entered, inFlight, peak atomic.Int64
	fn := func(c context.Context) {
		n := entered.Add(1)
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); cur > old && !peak.CompareAndSwap(old, cur); old = peak.Load() {
		}
		if n == 1 {
			time.Sleep(time.Second) // ignores its context
			return
		}
		<-c.Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Family(SweeperFamilyRuns).Go(ctx, fn)
	waitFor(t, "fn to start", 5*time.Second, func() bool { return entered.Load() == 1 })
	l.Family(SweeperFamilyRuns).Resign()
	for inFlight.Load() == 1 && entered.Load() == 1 {
		if _, _, end, ok := l.Family(SweeperFamilyRuns).Join(); ok {
			end()
			t.Fatal("a per-tick Join joined the family while its stuck unit still ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := entered.Load(); n != 1 {
		t.Fatalf("fn was entered %d times while its first run lived; want once", n)
	}
	waitFor(t, "the family to be taken again and fn run a second time", 5*time.Second, func() bool { return entered.Load() == 2 })
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d runs of fn at once; want 1", p)
	}
}

// TestFamilyFreedTakenWithoutSpreadingWait_PG: every family freed by a lost leader is taken at the next round,
// whatever the taker already leads, and a replica whose own election session is lost (a Postgres restart drops every session
// at once) retakes every family it held in its first round after redialling, not one per spreading wait.
func TestFamilyFreedTakenWithoutSpreadingWait_PG(t *testing.T) {
	// The spreading wait would leave n1's second family until 3*retry after n2 took its first, and the retake below
	// until 6*retry; each bound stays under those and leaves a second or more for a loaded runner.
	const retry = time.Second
	probe := pgPool(t)
	n1, n2 := famLeader(t, "replica-n1", retry), famLeader(t, "replica-n2", retry)
	stop1 := runLeader(t, n1)
	runLeader(t, n2)
	waitFor(t, "the two to share the four families", 15*time.Second, func() bool {
		return len(ledFamilies(n1)) > 0 && len(ledFamilies(n1))+len(ledFamilies(n2)) == len(SweeperFamilies)
	})
	time.Sleep(2 * retry) // n2's rounds try n1's families and find them held

	// n1 dies: its session ends, and so does its process.
	terminate(t, probe, electionPID(t, n1))
	stop1()
	start := time.Now()
	waitFor(t, "n2 to lead all four within two rounds", 2*retry+500*time.Millisecond, func() bool {
		return len(ledFamilies(n2)) == len(SweeperFamilies)
	})
	t.Logf("n2 took n1's families in %s", time.Since(start))

	// n2's own session ends: it retakes all four in its first round after redialling.
	before := familyEpochs(t, probe)
	terminate(t, probe, electionPID(t, n2))
	start = time.Now()
	waitFor(t, "n2 to notice the loss", 3*time.Second, func() bool { return len(ledFamilies(n2)) < len(SweeperFamilies) })
	waitFor(t, "n2 to retake all four in one round", n2.monitor+retry+time.Second, func() bool {
		return len(ledFamilies(n2)) == len(SweeperFamilies)
	})
	t.Logf("n2 retook its families in %s", time.Since(start))
	after := familyEpochs(t, probe)
	for _, f := range SweeperFamilies {
		if after[f] <= before[f] {
			t.Errorf("family %q epoch %d -> %d; want a new term", f, before[f], after[f])
		}
	}
}
