// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run its runner contained while it was offline (an outage loss whose stop_proxy the runner applied on
// reconnect) is held: the runner refuses it any lease. A revive brings it back. The first revive queues
// the runner's recover_containment and ends its session, the runner applies it on its next session, and the
// next revive leases the run and replaces its proxy.
func TestPG_RunnerLease_ReviveRecoversAContainedRun(t *testing.T) {
	f := newRunnerLeaseFixture(t)
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
