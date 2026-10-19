// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run its runner contained while it was offline (an outage loss whose stop_proxy the runner applied on
// reconnect) is held: the runner refuses it any lease. A revive brings it back. The first revive queues
// the runner's recover_containment and ends its session, the runner applies it on its next session, and the
// next revive leases the run and replaces its proxy.
func TestPG_RunnerLease_ReviveRecoversAContainedRun(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.heldRun(t)

	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", false); rerr == nil || rerr.reason != "revive_containment_recovering" {
		t.Fatalf("first revive of the held run = %+v, want revive_containment_recovering", rerr)
	}
	f.waitOffline(t) // the revive ended the runner's session, so its next one carries the recovery
	f.disconnect = f.connect(t)
	var outcome string
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT outcome FROM runner_pending_actions WHERE run_id = $1 AND kind = 'recover_containment'`, run.ID).Scan(&outcome); err != nil || outcome != types.RunnerActionSuccess {
		t.Fatalf("recover_containment outcome = %q, %v; want success", outcome, err)
	}
	if f.held(t, run) {
		t.Fatal("the run is still held after its runner applied the recovery")
	}

	if stored, err = f.st.GetRun(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", false); rerr != nil {
		t.Fatalf("revive after the recovery = %+v", rerr)
	}
	replaced := slices.ContainsFunc(f.fake.Calls, func(c string) bool {
		return strings.HasPrefix(c, "replace_proxy ") && strings.Contains(c, run.ID.String())
	})
	if !replaced {
		t.Fatalf("calls %v: want the recovered run's proxy replaced", f.fake.Calls)
	}
}

// A recovery the runner defers (a call on the run was still in flight when it applied it) is final for its
// own action and fences the run until it settles. The next revive queues a successor in the same obligation;
// the runner's success on it settles the deferred link, lifts the fence, and the revive after it leases.
func TestPG_RunnerLease_ReviveContinuesADeferredRecovery(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.heldRun(t)
	f.reviveRecovering(t, run)
	var first uuid.UUID
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT id FROM runner_pending_actions WHERE run_id = $1 AND kind = 'recover_containment'`, run.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// What the runner answers with a mutation of the run in flight (the offline package pins that side).
	prepare := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	if _, _, err := f.st.RecordRunnerActionResult(f.ctx, f.rn.ID, run.ID, first, types.RunnerActionDeferred, "", time.Now(), prepare); err != nil {
		t.Fatal(err)
	}
	f.waitOffline(t)
	f.disconnect = f.connect(t)
	if fenced, err := f.st.RunHasPendingRunnerAction(f.ctx, run.ID); err != nil || !fenced || !f.held(t, run) {
		t.Fatalf("after a deferred recovery: fenced %v (%v), held %v; want both", fenced, err, f.held(t, run))
	}

	f.reviveRecovering(t, run)
	f.waitOffline(t)
	f.disconnect = f.connect(t)
	var outcome, settlement string
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT s.outcome, d.settlement FROM runner_pending_actions s JOIN runner_pending_actions d ON d.id = s.supersedes
		WHERE d.id = $1 AND s.obligation_id = d.obligation_id`, first).Scan(&outcome, &settlement); err != nil || outcome != types.RunnerActionSuccess || settlement != "chain" {
		t.Fatalf("successor outcome %q, deferred link settlement %q, %v; want success and chain", outcome, settlement, err)
	}
	if fenced, err := f.st.RunHasPendingRunnerAction(f.ctx, run.ID); err != nil || fenced || f.held(t, run) {
		t.Fatalf("after the successor: fenced %v (%v), held %v; want neither", fenced, err, f.held(t, run))
	}
	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", false); rerr != nil {
		t.Fatalf("revive after the continued recovery = %+v", rerr)
	}
}

// Containment and its recovery are ordered by delivery (queue_seq, migration 0150), never by queued_at,
// each queuing transaction's start, which ties and can invert. A recovery delivered before the run's
// containment but stamped after it has not released that containment: the run is held, and a revive
// queues its recovery.
func TestPG_RunnerLease_ContainmentFollowsDeliveryOrder(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.heldRun(t)
	tx, err := f.st.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(f.ctx)) //nolint:errcheck // a no-op after Commit
	// A test-only forgery of the inverted stamp: the row triggers forbid writing one.
	if _, err := tx.Exec(f.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	early := uuid.New()
	if _, err := tx.Exec(f.ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, queued_at, applied_at, outcome,
			observed_at, binding_version, owner, execution_generation, proof, subject, obligation_id, queue_seq) OVERRIDING SYSTEM VALUE
		SELECT $2, runner_id, run_id, 'recover_containment', ref, queued_at + interval '1 second', applied_at, 'success',
			observed_at, 1, owner, execution_generation, '{}'::jsonb, 'run', $2, -1
		FROM runner_pending_actions WHERE run_id = $1 AND kind = 'stop_proxy'`, run.ID, early); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !f.held(t, run) {
		t.Fatal("a recovery delivered before the containment released it")
	}
	f.reviveRecovering(t, run)
}

// reviveRecovering revives the held run and wants the revive to queue (or find) its recovery.
func (f *runnerLeaseFixture) reviveRecovering(t *testing.T, run types.AgentRun) {
	t.Helper()
	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", false); rerr == nil || rerr.reason != "revive_containment_recovering" {
		t.Fatalf("revive of the held run = %+v, want revive_containment_recovering", rerr)
	}
}

// heldRun is a running run on the fixture's runner whose outage loss the runner contained on reconnect: a
// settled stop_proxy, so the runner refuses it any lease.
func (f *runnerLeaseFixture) heldRun(t *testing.T) types.AgentRun {
	t.Helper()
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	if err := f.srv.keepRunProxyConfig(f.ctx, run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081"}); err != nil {
		t.Fatal(err)
	}
	f.disconnect()
	f.waitOffline(t)
	if lost, err := f.st.MarkRunLost(f.ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !lost {
		t.Fatalf("mark lost = %v, %v", lost, err)
	}
	if _, _, err := f.st.QueueRunnerTeardown(f.ctx, f.rn.ID, types.RunnerActionStopProxy, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	f.disconnect = f.connect(t)
	if !f.contained(run) {
		t.Fatal("the runner never applied the outage loss's stop_proxy")
	}
	if !f.held(t, run) {
		t.Fatal("the contained run is not held")
	}
	return run
}

func (f *runnerLeaseFixture) held(t *testing.T, run types.AgentRun) bool {
	t.Helper()
	held, err := f.st.RunHeldContainedOnRunner(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func (f *runnerLeaseFixture) waitOffline(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the runner's session never ended")
		}
	}
}
