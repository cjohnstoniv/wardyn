// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunnerAdmissionPG_ThousandRunInventoryReachesReady(t *testing.T) {
	f := newAdmissionFixture(t)
	rows := []runnerwire.RunState{f.liveRow()}
	// 1000 live runs are more than any runner's max_runs admits, so they are seeded past placement: each is
	// recorded with no runner and then bound to this one. The test pins the inventory, not admission.
	for range 1000 {
		run := newFinalizeRun()
		run.CreatedBy, run.Placement = f.rn.Owner, types.PlacementLocal
		run, err := f.st.CreateRun(f.ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET runner_id = $2 WHERE id = $1`, run.ID, f.rn.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.st.SetSandboxRef(f.ctx, run.ID, "runner:"+f.rn.ID.String()+"/"+run.ID.String()); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, runnerwire.RunState{RunID: run.ID, SandboxPresent: true})
	}
	s := f.connect(t, nil, rows...)
	if !f.waitReady(s) {
		t.Fatalf("a 1001-run inventory never reached READY: %v", s.result())
	}
	if complete, needs := f.inventory(t); !complete || needs {
		t.Fatalf("inventory complete=%v needs_reconciliation=%v", complete, needs)
	}
	var unseen int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runner_inventory_candidates WHERE runner_id=$1 AND NOT seen`, f.rn.ID).Scan(&unseen); err != nil || unseen != 0 {
		t.Fatalf("%d of 1001 candidates unseen (%v)", unseen, err)
	}
}

// The production Admission's StatePage is bound to its session: a page from a
// session that a newer BeginInventory replaced is refused at the store fence.
func TestRunnerAdmissionPG_ReplacedSessionPageRefused(t *testing.T) {
	f := newAdmissionFixture(t)
	a, err := f.srv.admitRunner(f.ctx, f.rn)
	if err != nil {
		t.Fatal(err)
	}
	old, current := uuid.NewString(), uuid.NewString()
	if err := a.BeginInventory(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginInventory(f.ctx, current); err != nil {
		t.Fatal(err)
	}
	page := runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{f.liveRow()}, Final: true}
	if err := a.StatePage(f.ctx, old, page); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("a replaced session's page = %v, want refused", err)
	}
	if complete, _ := f.inventory(t); complete {
		t.Fatal("a replaced session's page completed the current inventory")
	}
	if err := a.StatePage(f.ctx, current, page); err != nil {
		t.Fatalf("the current session's page = %v", err)
	}
}

// A slow offline sweep runs off the Hub's session slot: the reconnect that
// triggers it reaches PENDING while the sweep is still blocked.
func TestRunnerAdmissionPG_SlowOfflineSweepDoesNotBlockReconnect(t *testing.T) {
	f := newAdmissionFixture(t)
	first := f.connect(t, nil, f.liveRow())
	if !f.waitReady(first) {
		t.Fatalf("first session never reached READY: %v", first.result())
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	// A NO KEY UPDATE hold makes the sweep's FOR UPDATE loss claim wait while
	// the reconnect's inventory, which takes only key-share locks, can proceed.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // released below; a no-op then
	if _, err := tx.Exec(f.ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR NO KEY UPDATE`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	first.cancel()
	<-first.done
	// Inside the resume window the session keeps its READY, but a dropped
	// peer is not online.
	for deadline := time.Now().Add(2 * time.Second); f.srv.RunnerOnline(f.rn.ID); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a dropped runner reads online")
		}
	}
	reached := make(chan struct{})
	var once sync.Once
	second := f.connect(t, func() { once.Do(func() { close(reached) }) }, f.liveRow())
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		_ = tx.Rollback(f.ctx)
		t.Fatalf("the reconnect waited on the offline sweep: %v", second.result())
	}
	var lostAt *time.Time
	if err := tx.QueryRow(f.ctx, `SELECT lost_at FROM agent_runs WHERE id=$1`, f.run.ID).Scan(&lostAt); err != nil {
		t.Fatal(err)
	}
	if lostAt != nil {
		t.Fatal("the sweep ran past the held run row")
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if got, _ := f.st.GetRun(f.ctx, f.run.ID); got.LostAt != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the offline sweep never marked the lapsed run lost")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunnerOfflineSweepsCoalescePerRunner(t *testing.T) {
	var q runnerOfflineSweeps
	release := make(chan struct{})
	var runs, other atomic.Int32
	id := uuid.New()
	done := make(chan struct{}, 8)
	sweep := func(context.Context, uuid.UUID) error {
		if runs.Add(1) == 1 {
			<-release
		}
		done <- struct{}{}
		return nil
	}
	q.schedule(t.Context(), id, sweep)
	for range 5 {
		q.schedule(t.Context(), id, sweep)
	}
	q.schedule(t.Context(), uuid.New(), func(context.Context, uuid.UUID) error { other.Add(1); return nil })
	close(release)
	<-done
	<-done
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 2 {
		t.Fatalf("%d sweeps for one runner, want the running one plus one coalesced rerun", runs.Load())
	}
	if other.Load() != 1 {
		t.Fatal("another runner's sweep waited behind this runner's")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.again) != 0 {
		t.Fatal("a finished runner kept its sweep slot")
	}
}

func TestRunnerAdmissionPG_RevokeKillsRunsThenClosesTheSession(t *testing.T) {
	f := newAdmissionFixture(t)
	reg := &registryRunner{fakeRunner: &fakeRunner{}}
	f.srv.cfg.Runner = reg
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	// A revoke that cannot commit restores the route. The hold is on a pool of its own: the revoke
	// asks f.pool for a connection on this goroutine (WARDYN_TEST_POOL_GUARD).
	holder, err := pgxpool.New(f.ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // released below; a no-op then
	if _, err := tx.Exec(f.ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR UPDATE`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
	if _, _, err := f.srv.revokeRunner(short, f.rn.ID, types.ActorHuman, "security-admin", ""); err == nil {
		t.Fatal("a blocked revoke reported success")
	}
	cancel()
	_ = tx.Rollback(f.ctx)
	name := placement.RunnerSubstrateName(f.rn.ID.String())
	reg.mu.Lock()
	rerouted := len(reg.added) == 2 && reg.added[1] == name && len(reg.removed) == 1
	reg.mu.Unlock()
	if !rerouted {
		t.Fatalf("a failed revoke did not restore routing: added=%v removed=%v", reg.added, reg.removed)
	}

	if !f.srv.RunnerOnline(f.rn.ID) {
		t.Fatal("a READY runner reads offline")
	}
	rev, _, err := f.srv.revokeRunner(f.ctx, f.rn.ID, types.ActorHuman, "security-admin", "")
	if err != nil || len(rev.Killed) != 1 || rev.Killed[0].ID != f.run.ID {
		t.Fatalf("revoke = %+v, %v", rev, err)
	}
	if got, _ := f.st.GetRun(f.ctx, f.run.ID); got.State != types.RunKilled {
		t.Fatalf("run on the revoked runner is %s, want KILLED", got.State)
	}
	// The run's kill cascade ran after the commit: credentials revoked, no
	// teardown sent to the revoked runner, and the reason and the runner's
	// state at revocation recorded for the run's failure block.
	audit := f.srv.cfg.Audit.(*syncAudit)
	kills := audit.eventsFor(f.run.ID, "run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" || kills[0].Actor != "security-admin" {
		t.Fatalf("run.kill rows = %+v, want one success by the revoker", kills)
	}
	var data map[string]any
	if err := json.Unmarshal(kills[0].Data, &data); err != nil || data["reason"] != "runner_revoked" || data["runner_online"] != true {
		t.Fatalf("run.kill data = %s (%v)", kills[0].Data, err)
	}
	if !f.revokeAudited(t, "security-admin", "") {
		t.Fatal("no runner.revoke row")
	}
	if f.srv.RunnerOnline(f.rn.ID) {
		t.Fatal("a revoked runner reads online")
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked runner's session stayed open")
	}
	if !errors.Is(s.err, runnerwire.ErrRevoked) {
		t.Fatalf("session ended with %v, want REVOKED", s.err)
	}
	if f.srv.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("a fresh endpoint for the revoked runner is online")
	}
	reg.mu.Lock()
	removed := len(reg.removed) >= 2 && reg.removed[len(reg.removed)-1] == name
	reg.mu.Unlock()
	if !removed {
		t.Fatal("the revoked runner's substrate stayed routed")
	}
	again := f.connect(t, nil, f.liveRow())
	if f.waitReady(again) || !errors.Is(again.result(), runnerwire.ErrRevoked) {
		t.Fatalf("a revoked runner reconnected: %v", again.result())
	}
	if err := f.srv.runnerEndpoint(f.rn.ID).sub.KillSandbox(f.ctx, f.run.SandboxRef); errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatal("a teardown queued against a revoked runner")
	}
}

// Switching runners off ends the live session, and every create on a runner
// reads the stored switch first, even on an endpoint admitted while it was on.
func TestRunnerAdmissionPG_RunnersOffEndsSessionsAndRefusesDispatch(t *testing.T) {
	f := newAdmissionFixture(t)
	if _, err := f.st.PutSiteConfig(f.ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	sub := f.srv.runnerEndpoint(f.rn.ID).sub
	spec := runner.SandboxSpec{RunID: uuid.New(), ConfinementClass: types.CC1}
	if _, err := sub.CreateSandbox(f.ctx, spec); errors.Is(err, errRunnersDisabled) {
		t.Fatal("a create refused as disabled while runners are on")
	}
	if _, err := f.st.PutSiteConfig(f.ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.CreateSandbox(f.ctx, spec); !errors.Is(err, errRunnersDisabled) {
		t.Fatalf("create with runners off = %v, want refused before the wire", err)
	}
	f.srv.RunnersDisabled(f.ctx)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived the switch")
	}
	if f.srv.RunnerOnline(f.rn.ID) {
		t.Fatal("a runner reads online after runners were switched off")
	}
}

// The offline sweep's loss claim waits for a held run row before it takes the runner row, so a write
// to the runner (a reconnect's TouchRunner) never queues behind the run row's holder, and the claim
// still lands once the row is free.
func TestRunnerAdmissionPG_LossClaimHoldsNoRunnerLockWhileItWaitsForTheRun(t *testing.T) {
	f := newAdmissionFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // released below; a no-op then
	if _, err := tx.Exec(f.ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR NO KEY UPDATE`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	type claim struct {
		applied bool
		err     error
	}
	claimed := make(chan claim, 1)
	go func() {
		applied, err := f.st.MarkRunnerRunLost(f.ctx, f.rn.ID, f.run.ID, f.run.SandboxRef, time.Now())
		claimed <- claim{applied, err}
	}()
	// A second client, off the pool this goroutine already holds a connection of: the observer and
	// the runner's writer. The claim is waiting once a backend waits on a row lock.
	other, err := pgx.ConnectConfig(f.ctx, f.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background()) //nolint:errcheck // test teardown
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting bool
		if err := other.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND query LIKE '%agent_runs%FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loss claim never waited on the held run row")
		}
	}
	if _, err := other.Exec(f.ctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(f.ctx, `UPDATE runners SET last_seen_at = now() WHERE id = $1`, f.rn.ID); err != nil {
		t.Errorf("a write to the runner waited behind the loss claim's wait on the run row: %v", err)
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if c := <-claimed; c.err != nil || !c.applied {
		t.Fatalf("the loss claim after the row was freed = %v, %v; want applied", c.applied, c.err)
	}
}
