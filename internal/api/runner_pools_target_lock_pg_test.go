// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A detach of a remote-hosted runner takes the pool's use-policy target lock first, whether or not it
// refuses a governed pool: a policy write or approval holding that lock finishes before the detach reads
// the pool. Without it the detach is ordered by the pool row alone, which a policy write takes later.
func TestPG_RunnerPoolConcurrentDetachWaitsOnThePolicyTargetLock(t *testing.T) {
	for _, refuseGoverned := range []bool{true, false} {
		e := newPoolEnv(t, false)
		a := e.createPool("A", types.RunnerPoolOrganization)
		if w := e.admin(http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
			t.Fatalf("attach = %d %s", w.Code, w.Body)
		}
		ctx := context.Background()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.LockGovernanceTarget(ctx, tx, types.GovernanceTargetRunnerPoolUsePolicy, a.ID.String()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := e.pg.RemoveRunnerPoolExecutor(ctx, a.ID, "fake", refuseGoverned)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("refuseGoverned=%v: the detach returned (%v) while the policy target lock was held", refuseGoverned, err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("refuseGoverned=%v: the detach after the lock was released = %v", refuseGoverned, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("refuseGoverned=%v: the detach did not finish after the lock was released", refuseGoverned)
		}
	}
}

// An attach decided as direct on a catalogue read is decided again under the row locks of every pool
// holding the executor: a use policy approved on another pool in between commits first, and the attach
// is then held, never applied directly. Before the race, with B holding the executor and no policy
// anywhere, an attach to C applies directly. Then a second connection approving B's policy holds B's
// target and row locks; the attach to A waits on them and, once B's policy commits, is held.
func TestPG_RunnerPoolAttachWaitsOnAnotherPoolsPolicyApproval(t *testing.T) {
	e := newPoolEnv(t, true)
	org := types.RunnerPoolOrganization
	a, b, c := e.createPool("A", org), e.createPool("B", org), e.createPool("C", org)
	for _, p := range []types.RunnerPool{b, c} {
		if w := e.call(e.super, http.MethodPut, poolPath(p.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK || e.members(p.ID) != 1 {
			t.Fatalf("attach to %s with no policy anywhere = %d %s, want a direct 200", p.Name, w.Code, w.Body)
		}
	}
	ctx := t.Context()
	other, err := testutil.PGConn(t, e.pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) //nolint:errcheck // committed below; a no-op then
	if err := store.LockGovernanceTarget(ctx, other, types.GovernanceTargetRunnerPoolUsePolicy, b.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRunnerPoolUsePolicyQ(ctx, other, b.ID, nil,
		[]types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: "sub-named"}}, "sub-other-approver"); err != nil {
		t.Fatal(err)
	}
	done := e.attachWaiting(a, "%ORDER BY p.id FOR UPDATE%")
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e.attachHeld(<-done, a)
}

// attachWaiting starts super's attach of remote-hosted runner fake to pool p and returns its answer's
// channel once the attach waits on a lock in a statement matching like.
func (e *poolEnv) attachWaiting(p types.RunnerPool, like string) <-chan *httptest.ResponseRecorder {
	e.t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.call(e.super, http.MethodPut, poolPath(p.ID)+"/remote-runners/fake", "") }()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND query LIKE $1`, like).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n > 0 {
			return done
		}
		select {
		case w := <-done:
			e.t.Fatalf("the attach to %s answered %d %s without waiting", p.Name, w.Code, w.Body)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the attach to %s never waited on a lock in %q", p.Name, like)
		}
	}
}

// attachHeld checks that answer w held the attach to pool p and added no member.
func (e *poolEnv) attachHeld(w *httptest.ResponseRecorder, p types.RunnerPool) {
	e.t.Helper()
	held := e.pending(w)
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.Op != opExecutorAttach || held.TargetKey != p.ID.String() {
		e.t.Errorf("held = %+v, want the attach to %s", held, p.Name)
	}
	if n := e.members(p.ID); n != 0 {
		e.t.Errorf("%s holds %d members after a held attach, want 0", p.Name, n)
	}
}

// Two direct attaches of one remote-hosted runner, to open pool A and to pool B under a use policy,
// are serialized on the runner: the attach to A waits for the one to B, then reads B holding it under
// the policy and is held. Each locks only its own pool and the pools holding the runner when it reads,
// so without the runner's lock neither would see the other's uncommitted member row, and the runner
// would end up in open A beside B's policy without a second human.
func TestPG_RunnerPoolConcurrentAttachesSerializeOnTheRunner(t *testing.T) {
	e := newPoolEnv(t, true)
	org := types.RunnerPoolOrganization
	a, b := e.createPool("A", org), e.createPool("B", org)
	ctx := t.Context()
	if _, err := e.pg.PutRunnerPoolUsePolicy(ctx, b.ID, nil,
		[]types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: "sub-named"}}, "sub-setup"); err != nil {
		t.Fatal(err)
	}
	other, err := testutil.PGConn(t, e.pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) //nolint:errcheck // committed below; a no-op then
	// The attach to B as AddRunnerPoolExecutor takes it, left uncommitted.
	if err := store.LockGovernanceTarget(ctx, other, types.GovernanceTargetRunnerPoolUsePolicy, b.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.LockRemoteRunnerPoolMembershipQ(ctx, other, "fake"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.AddRunnerPoolExecutorQ(ctx, other, b.ID, "fake"); err != nil {
		t.Fatal(err)
	}
	done := e.attachWaiting(a, "%pg_advisory_xact_lock%")
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e.attachHeld(<-done, a)
	if n := e.members(b.ID); n != 1 {
		t.Errorf("B holds %d members, want the runner", n)
	}
}
