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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		runs[i], _, errs[i] = srv.launchHarnessLoginRun(context.Background(), actor, hl, perUserPortal, awsSSOPin{}, awsSSOScope{})
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
