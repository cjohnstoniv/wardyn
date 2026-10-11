// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"testing"
)

func TestLostLocalHeadlessQueuesContainmentWithoutTearingDown(t *testing.T) {
	f := newLostFixture(t)
	f.st.run.Interactive = false
	f.st.run.Placement = types.PlacementLocal
	f.run = f.st.run
	f.ls.lapsed = true
	f.lr.proxyErr = runner.ErrPendingOnRunner
	f.sweepTokens(t)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 || f.lr.proxyStopCount() != 1 {
		t.Fatalf("offline local headless run torn down: state=%s stop=%d proxy=%d", f.st.State(), f.rn.stopCount(), f.lr.proxyStopCount())
	}
	rows := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	data := leaseAuditData(t, rows[0])
	if rows[0].Outcome != "success" || data["containment"] != "pending_runner" || data["kept"] != true || data["lost_error"] != nil {
		t.Fatalf("queued containment misreported: %+v %v", rows[0], data)
	}
	if f.st.containmentError() != "" {
		t.Fatal("queued action marked as daemon failure")
	}
}
func TestLostHeadlessClusterStillUsesExistingTeardownPolicy(t *testing.T) {
	f := newLostFixture(t)
	f.run.Interactive = false
	f.run.Placement = types.PlacementRemote
	if f.srv.lostRunKeepable(f.run, f.srv.cfg.Now()) {
		t.Fatal("cluster headless run became keepable")
	}
}
