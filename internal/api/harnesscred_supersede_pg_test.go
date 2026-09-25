// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The one interleaving the deterministic supersede order could not close, against
// a REAL Postgres: two concurrent sign-ins by one person, one of them stalled
// between stamping its run and inserting it, must still leave exactly ONE live
// sign-in sandbox. Two would be two boxes each able to capture a ~1yr AWS SSO
// session — the defect the per-person advisory lock (store.LoginLocker) exists
// to remove.
//
// Postgres-backed on purpose, and not reducible to a fake: the lock IS a
// Postgres session-level advisory lock, so a double that answers
// LockLoginSupersede with a Go mutex would prove the test, not the product.
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

package api

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// loginRaceWindow is how long the SECOND sign-in is given to get as far as it
// can while the first is parked inside CreateRun. Unlocked that is far enough
// to insert its row and run its own supersede pass (a handful of statements
// against a local database); locked, it spends the whole window waiting on the
// advisory lock. Generous rather than tight — the assertion is about WHAT
// survives, and a window that is too long only makes the test slower.
const loginRaceWindow = 300 * time.Millisecond

// loginRaceStore is the real PG store with ONE seam: the first CreateRun parks
// between the caller's in-process created_at stamp (newStepRun) and the INSERT.
//
// That gap is the whole defect. created_at is stamped before the insert, so a
// launch that stalls there inserts a row that is OLDER than a sibling which
// started later — and loginRunPrecedes, correctly, then refuses to let the
// later-inserted sibling supersede it. A stall of this shape is not exotic: a
// second replica with a skewed clock, a GC pause, or a slow round trip does it.
type loginRaceStore struct {
	store.PG
	creates atomic.Int32
	entered chan struct{} // closed when the FIRST CreateRun has parked
	proceed chan struct{} // closed by the test to let it insert
}

func (s *loginRaceStore) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	if s.creates.Add(1) == 1 {
		close(s.entered)
		<-s.proceed
	}
	return s.PG.CreateRun(ctx, r)
}

// TestPG_LoginSupersedeSerializesConcurrentSignIns is the defect and its fix.
//
// RED with the lock removed (the two lockLoginSupersede calls deleted): both
// launches survive, because the stalled one's created_at precedes the other's
// and neither pass ever sees a run it is allowed to end.
func TestPG_LoginSupersedeSerializesConcurrentSignIns(t *testing.T) {
	pool := throwawayPGPool(t)
	st := &loginRaceStore{
		PG:      store.NewPG(pool),
		entered: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	// The seam is what serializes the launches; without it this test would
	// silently measure the unlocked tree and pass for the wrong reason.
	if _, ok := any(st).(store.LoginLocker); !ok {
		t.Fatal("the pg-backed store under test does not implement store.LoginLocker — nothing is serialized")
	}

	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	// memAudit, not the harness's own recorder: two launches audit CONCURRENTLY
	// here, and recRecorder appends without a mutex.
	cfg.Audit = &memAudit{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("aws-sso harness login convention missing")
	}
	const actor = "member@corp.example"

	var wg sync.WaitGroup
	runs := make([]types.AgentRun, 2)
	errs := make([]error, 2)
	launch := func(i int) {
		defer wg.Done()
		runs[i], _, errs[i] = srv.launchHarnessLoginRun(context.Background(), actor, hl, loginTarget{startURL: perUserPortal})
	}

	// First sign-in: runs until it is parked inside CreateRun, holding the lock.
	wg.Add(1)
	go launch(0)
	select {
	case <-st.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first sign-in never reached CreateRun")
	}

	// Second sign-in, started while the first is mid-launch — the concurrency
	// the field report describes (a double-click, or the console and a wdn_
	// token on the same subject).
	wg.Add(1)
	go launch(1)
	time.Sleep(loginRaceWindow)
	close(st.proceed)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("sign-in %d failed to launch: %v", i, err)
		}
	}

	live, err := st.ActiveRunsByCreator(context.Background(), actor, harnessLoginTask, awsSSOAgent)
	if err != nil {
		t.Fatalf("read this person's live sign-in sandboxes: %v", err)
	}
	if len(live) != 1 {
		ids := make([]string, 0, len(live))
		for _, r := range live {
			ids = append(ids, r.ID.String()+"="+string(r.State))
		}
		t.Fatalf("live sign-in sandboxes = %d %v, want exactly 1 — two concurrent sign-ins left two live "+
			"credential-bearing sandboxes, each able to capture an AWS SSO session (launched %s, %s)",
			len(live), ids, runs[0].ID, runs[1].ID)
	}
	if live[0].ID != runs[0].ID && live[0].ID != runs[1].ID {
		t.Fatalf("the survivor %s is neither sign-in (%s, %s)", live[0].ID, runs[0].ID, runs[1].ID)
	}
}

// sizedPool reopens base's database with an explicit pool_max_conns, so a test
// can stand in a deployment sized the way docs/ENV.md's WARDYN_PG_DSN row
// actually permits ("at least 2, and at least 4 with the ground-truth rotator
// enabled") rather than the double-digit default a developer box produces.
func sizedPool(t *testing.T, base *pgxpool.Pool, maxConns int) *pgxpool.Pool {
	t.Helper()
	dsn := base.Config().ConnString()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Connect(context.Background(), fmt.Sprintf("%s%spool_max_conns=%d", dsn, sep, maxConns))
	if err != nil {
		t.Fatalf("open a pool_max_conns=%d pool: %v", maxConns, err)
	}
	// Before throwawayPGPool's DROP DATABASE cleanup, which LIFO ordering gives
	// us for free by registering this one later.
	t.Cleanup(pool.Close)
	if got := pool.Config().MaxConns; got != int32(maxConns) {
		t.Fatalf("pool max_conns = %d, want %d — the DSN parameter did not take, so this test would prove nothing", got, maxConns)
	}
	return pool
}

// TestPG_LoginSupersedeDoesNotStarveConcurrentSignIns is the availability half,
// and it is the case the same-actor race test above cannot reach: two sign-ins
// by DIFFERENT people. Their actor strings fold to different objids, so they
// never contend on the lock at all — what they contend for is the POOL.
//
// The failure this pins is daemon-wide. A lock hold borrows a connection for
// its whole span and the guarded work needs another, so one connection per
// concurrent sign-in can exhaust the pool; `lock_timeout` does not bound a pool
// acquire (it bounds a lock wait), and pgxpool's Acquire does not error on an
// empty pool, it blocks on the context. With no WriteTimeout and no
// TimeoutHandler in front of the route, that context ends only when the client
// disconnects — so both sign-ins, and every other database-backed request in
// the daemon, would hang. At pool_max_conns=3 two people are enough; at the
// floor docs/ENV.md blesses, 2, one is.
//
// Sized at both, with the single-instance lock held exactly as a serving
// wardynd holds it (one connection, whole process lifetime) — that hold is what
// makes the arithmetic as tight as it is in production.
func TestPG_LoginSupersedeDoesNotStarveConcurrentSignIns(t *testing.T) {
	for _, maxConns := range []int{2, 3} {
		t.Run(fmt.Sprintf("pool_max_conns=%d", maxConns), func(t *testing.T) {
			pool := sizedPool(t, throwawayPGPool(t), maxConns)

			// The daemon's own process-lifetime hold, taken the way cmd/wardynd
			// takes it. Without this the pool is a connection richer than any
			// serving deployment's and the starvation cannot reproduce.
			releaseInstance, ok, err := db.TryAdvisoryLock(context.Background(), pool, db.SingleInstanceLockKey)
			if err != nil {
				t.Fatalf("take the single-instance lock: %v", err)
			}
			if !ok {
				t.Fatal("the single-instance lock was already held on a throwaway database")
			}
			defer releaseInstance()

			h := newHarness(t)
			cfg := baseTestConfig(h, store.NewPG(pool))
			cfg.Audit = &memAudit{}
			cfg.Approvals = h.approvals
			cfg.Broker = h.broker
			cfg.Runner = &fakeRunner{}
			cfg.Secrets = &memSecrets{m: map[string][]byte{}}
			cfg.MaskRegistry = secretmask.NewRegistry()
			cfg.BedrockRegion = "us-east-1"
			cfg.DefaultPolicy = govDeployment()
			srv := New(cfg)

			hl, ok := agentHarnessLogin(awsSSOAgent)
			if !ok {
				t.Fatal("aws-sso harness login convention missing")
			}
			actors := []string{"alice@corp.example", "bob@corp.example"}

			var wg sync.WaitGroup
			errs := make([]error, len(actors))
			elapsed := make([]time.Duration, len(actors))
			for i, actor := range actors {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// A deadline stands in for the client that eventually gives up.
					// Wedged, the launch fails with it; healthy, it never comes close.
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					started := time.Now()
					_, _, errs[i] = srv.launchHarnessLoginRun(ctx, actor, hl, loginTarget{startURL: perUserPortal})
					elapsed[i] = time.Since(started)
				}()
			}
			wg.Wait()

			for i, actor := range actors {
				if errs[i] != nil {
					t.Errorf("%s could not sign in (%s): %v — the lock starved the work it guards", actor, elapsed[i], errs[i])
				}
				// Not merely "finished": finished PROMPTLY. The wedge resolves only
				// when a context dies, so a sign-in that took most of the budget is
				// the same defect wearing a shorter deadline.
				if elapsed[i] > 10*time.Second {
					t.Errorf("%s waited %s to sign in, want well under 10s", actor, elapsed[i])
				}
			}
		})
	}
}

// TestPG_LoginSupersedeRefusesWhenThePersonsLockIsHeld is #505 F5 against the
// real lock: another session holds this person's key past the wait budget, so
// the launch is REFUSED — errSignInBusy, no run row — rather than proceeding
// unlocked into the interleaving the lock exists to remove. Held with raw SQL
// on its own connection, not through db.AdvisoryLockKeyed, so the refusal comes
// from the Postgres lock wait itself and not from the in-process slot.
func TestPG_LoginSupersedeRefusesWhenThePersonsLockIsHeld(t *testing.T) {
	pool := throwawayPGPool(t)
	const actor = "member@corp.example"

	prev := db.LoginSupersedeLockWait
	db.LoginSupersedeLockWait = 500 * time.Millisecond
	t.Cleanup(func() { db.LoginSupersedeLockWait = prev })

	holder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the holder's connection: %v", err)
	}
	obj := int32(crc32.ChecksumIEEE([]byte(actor)))
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_lock($1, $2)`, db.LoginSupersedeLockClass, obj); err != nil {
		t.Fatalf("hold the person's sign-in lock: %v", err)
	}
	released := false
	release := func() {
		if !released {
			holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, db.LoginSupersedeLockClass, obj) //nolint:errcheck // test cleanup
			holder.Release()
			released = true
		}
	}
	t.Cleanup(release)

	h := newHarness(t)
	cfg := baseTestConfig(h, store.NewPG(pool))
	cfg.Audit = &memAudit{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("aws-sso harness login convention missing")
	}

	_, _, err = srv.launchHarnessLoginRun(context.Background(), actor, hl, loginTarget{startURL: perUserPortal})
	if !errors.Is(err, errSignInBusy) {
		t.Fatalf("launch err = %v, want errSignInBusy — a held lock must refuse, not proceed unlocked", err)
	}
	live, err := store.NewPG(pool).ActiveRunsByCreator(context.Background(), actor, harnessLoginTask, hl.agent)
	if err != nil {
		t.Fatalf("list the person's login runs: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a refused sign-in left %d login run(s) behind", len(live))
	}

	release()
	if _, _, err := srv.launchHarnessLoginRun(context.Background(), actor, hl, loginTarget{startURL: perUserPortal}); err != nil {
		t.Fatalf("with the lock free the sign-in must proceed: %v", err)
	}
}
