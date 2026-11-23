// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// After a run moves to another runner, the grant its first runner's state holds serves nothing for the old runner,
// and none for the new one until the old state is released; the new runner's state then reads the grants again.
func TestViaOrg_ARunnerTransferServesOnlyTheAuthoritativeRunner(t *testing.T) {
	setViaOrgSeams(t, viaOrgIdleTimeout, time.Hour, viaOrgMaxBytes) // no watcher tick: the test drives every release
	f := newViaOrgAPIFixture(t)
	r, r2 := f.rn.ID, *f.other.RunnerID
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK {
		t.Fatalf("the first forward = %d %s", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 1 {
		t.Fatalf("upstream saw %d requests, want 1", hits)
	}

	if _, err := f.pool.Exec(context.Background(), "UPDATE agent_runs SET runner_id = $1 WHERE id = $2", r2, f.run.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRunnerMismatch) {
		t.Fatalf("a forward relayed by the old runner = %d %s, want 403 relay_runner_mismatch", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 1 {
		t.Fatalf("the old runner's forward reached upstream: %d requests", hits)
	}
	// R's state still holds the grant: it is still R's state, so the new runner is refused by it.
	if w := f.forward(t, r2, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRunnerMismatch) {
		t.Fatalf("a forward relayed by the new runner while R's state is held = %d %s, want 403 relay_runner_mismatch", w.Code, w.Body)
	}

	// R's session ends. The watcher, on seeing the run moved, drops the state; this test does that release itself.
	f.srv.RecordRunnerDisconnected(context.Background(), r, nil)
	waitFor(t, "R's engine released", func() bool { return viaOrgEngineReleased(f.srv, f.run.ID) })
	f.srv.viaOrg.mu.Lock()
	state := f.srv.viaOrg.runs[f.run.ID]
	f.srv.viaOrg.mu.Unlock()
	if state == nil || state.runner != r {
		t.Fatalf("R's state = %+v, want R's", state)
	}
	f.srv.releaseViaOrgRun(state, true, false)

	grantsBefore := f.runs.grants.Load()
	if w := f.forward(t, r2, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK {
		t.Fatalf("a forward relayed by the new runner after R's state is gone = %d %s, want 200", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 2 {
		t.Fatalf("upstream saw %d requests, want 2", hits)
	}
	if got := f.runs.grants.Load(); got != grantsBefore+1 {
		t.Fatalf("grant reads went %d -> %d, want one more for the new state", grantsBefore, got)
	}
}

// A held grant serves nothing once the run cannot be read or has ended: the gate's own run read decides each request.
func TestViaOrg_AHeldGrantServesNothingOnceTheRunCannotBeReadOrEnds(t *testing.T) {
	setViaOrgSeams(t, viaOrgIdleTimeout, time.Hour, viaOrgMaxBytes)
	f := newViaOrgAPIFixture(t)
	r := f.rn.ID
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK {
		t.Fatalf("the first forward = %d %s", w.Code, w.Body)
	}
	hits := func() int { h, _, _ := f.up.seen(); return h }
	if hits() != 1 {
		t.Fatalf("upstream saw %d requests, want 1", hits())
	}

	f.runs.fail.Store(true)
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonInternalLivenessReadFailed {
		t.Fatalf("a forward while the run cannot be read = %d %s, want 503 %s", w.Code, w.Body, reasonInternalLivenessReadFailed)
	}
	if hits() != 1 {
		t.Fatalf("a forward that could not read the run reached upstream: %d requests", hits())
	}
	f.runs.fail.Store(false)
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK {
		t.Fatalf("the forward once the run reads again = %d %s, want 200", w.Code, w.Body)
	}
	if hits() != 2 {
		t.Fatalf("upstream saw %d requests, want 2", hits())
	}

	if ok, err := f.st.UpdateRunStateIf(context.Background(), f.run.ID, f.run.State, types.RunKilled); err != nil || !ok {
		t.Fatalf("end the run: %v %v", ok, err)
	}
	if w := f.forward(t, r, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != "run_terminal" {
		t.Fatalf("a forward for an ended run = %d %s, want 403 run_terminal", w.Code, w.Body)
	}
	if hits() != 2 {
		t.Fatalf("a forward for an ended run reached upstream: %d requests", hits())
	}
}
