// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/storehealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const lapseTestGrace = 25 * time.Minute

// censusStore is lostStore plus the lapse census read, answering a fixed census.
type censusStore struct {
	*lostStore
	census store.LapseCensus
	err    error
	reads  int
}

func (s *censusStore) LapseCensus(context.Context, time.Duration, time.Duration) (store.LapseCensus, error) {
	s.reads++
	return s.census, s.err
}

// healthyTracker is a tracker whose database has answered for at least span, read on a clock that stays put.
func healthyTracker(grace, span time.Duration) *storehealth.Tracker {
	const step = 20 * time.Second
	start := time.Now()
	clk := start
	tr := storehealth.New(func() time.Time { return clk }, grace)
	for clk.Sub(start) < span {
		tr.Observe(nil)
		clk = clk.Add(step)
	}
	return tr
}

// recoveringTracker has never seen a good ping, so it is recovering.
func recoveringTracker(grace time.Duration) *storehealth.Tracker {
	return storehealth.New(time.Now, grace)
}

// quietCensus is a census in which nothing marks an outage: ten runs, the newest renewal a minute old, one lapse.
var quietCensus = store.LapseCensus{Running: 10, NewestRenewAge: time.Minute, Lapsed: 1}

func newLapseFixture(t *testing.T, tr *storehealth.Tracker, census store.LapseCensus) (*lostFixture, *censusStore) {
	t.Helper()
	f := newLostFixture(t)
	cs := &censusStore{lostStore: f.ls, census: census}
	f.srv.cfg.Store = cs
	f.srv.cfg.StoreHealth = tr
	f.ls.lapsed = true
	return f, cs
}

func holdRows(a *syncAudit) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range a.snapshot() {
		if ev.Action == "run.lapse_sweep.hold" {
			out = append(out, ev)
		}
	}
	return out
}

func hasEvent(a *syncAudit, runID uuid.UUID, action string) bool {
	return len(a.eventsFor(runID, action)) > 0
}

func TestLapseVerdict_RecoveringIsOutageClass(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	v := f.srv.lapseVerdictNow(context.Background())
	if !v.outage || !slices.Equal(v.conditions, []string{"recovering"}) {
		t.Fatalf("verdict = %+v, want an outage with the condition recovering", v)
	}
}

func TestLapseVerdict_NoRenewalsForTwentyMinutesIsOutageClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		census store.LapseCensus
		want   bool
	}{
		{"one run, 20m1s", store.LapseCensus{Running: 1, NewestRenewAge: 20*time.Minute + time.Second}, true},
		{"one run, exactly 20m", store.LapseCensus{Running: 1, NewestRenewAge: 20 * time.Minute}, false},
		{"no run at all", store.LapseCensus{Running: 0, NewestRenewAge: time.Hour}, false},
	} {
		f, _ := newLapseFixture(t, healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute), tc.census)
		v := f.srv.lapseVerdictNow(context.Background())
		if v.outage != tc.want || (tc.want && !slices.Equal(v.conditions, []string{"no_renewals"})) {
			t.Errorf("%s: verdict = %+v, want outage %v", tc.name, v, tc.want)
		}
	}
}

func TestLapseVerdict_MassLapseIsOutageClass(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lapsed, total int
		want          bool
	}{
		{"5 of 20", 5, 20, true},
		{"4 of 8 is below the floor of 5", 4, 8, false},
		{"5 of 21 is below a quarter", 5, 21, false},
	} {
		f, _ := newLapseFixture(t, healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute),
			store.LapseCensus{Running: tc.total, NewestRenewAge: time.Minute, Lapsed: tc.lapsed})
		v := f.srv.lapseVerdictNow(context.Background())
		if v.outage != tc.want || (tc.want && !slices.Equal(v.conditions, []string{"mass_lapse"})) {
			t.Errorf("%s: verdict = %+v, want outage %v", tc.name, v, tc.want)
		}
	}
}

func TestLapseVerdict_OffWithNoTrackerOrZeroGrace(t *testing.T) {
	for name, tr := range map[string]*storehealth.Tracker{
		"no tracker": nil,
		"zero grace": recoveringTracker(0),
	} {
		f, cs := newLapseFixture(t, tr, store.LapseCensus{Running: 1, NewestRenewAge: time.Hour, Lapsed: 9})
		v := f.srv.lapseVerdictNow(context.Background())
		if v.outage || len(v.conditions) != 0 || cs.reads != 0 {
			t.Errorf("%s: verdict = %+v, census reads = %d; want no outage and no read", name, v, cs.reads)
		}
	}
}

func TestLapseVerdict_AFailedCensusReadIsOutageClass(t *testing.T) {
	f, cs := newLapseFixture(t, healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute), quietCensus)
	cs.err = errors.New("census down")
	v := f.srv.lapseVerdictNow(context.Background())
	if !v.outage || !slices.Equal(v.conditions, []string{"census_unread"}) {
		t.Fatalf("verdict = %+v, want an outage with the condition census_unread", v)
	}
}

func TestLapseVerdict_RecoveringReadsNoCensus(t *testing.T) {
	f, cs := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	f.srv.lapseVerdictNow(context.Background())
	if cs.reads != 0 {
		t.Fatalf("census reads = %d, want 0: a recovering server needs no census", cs.reads)
	}
}

// unattended makes the fixture's run a headless run on the server's own Docker.
func unattended(f *lostFixture) {
	f.st.run.Interactive = false
	f.run = f.st.run
}

func TestLostRun_AnUnattendedServerRunIsKeptInAnOutage(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	f.sweepTokens(t)

	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Fatalf("lost = %v %q, want the run marked lost (outage)", lostAt, reason)
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %d, want 1", len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != true || d["cause"] != "control_plane_outage" {
		t.Errorf("run.lost data = %v, want kept:true and cause control_plane_outage", d)
	}
	if f.lr.proxyStopCount() != 1 || f.rn.endCount() != 0 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Errorf("StopProxy %d, EndSandbox %d, StopSandbox %d, state %s; want 1, 0, 0, RUNNING",
			f.lr.proxyStopCount(), f.rn.endCount(), f.rn.stopCount(), f.st.State())
	}
	if hasEvent(f.audit, f.run.ID, "run.reconcile") {
		t.Error("a kept run wrote run.reconcile")
	}
	if rows := holdRows(f.audit); len(rows) != 1 || leaseAuditData(t, rows[0])["held"] != float64(1) {
		t.Errorf("run.lapse_sweep.hold rows = %+v, want one with held 1", rows)
	}
}

// TestLostRun_OneDeadProxyAmongHealthyRunsEndsAsToday: a healthy server whose one run's proxy died on its own
// ends that headless run as before (test T8).
func TestLostRun_OneDeadProxyAmongHealthyRunsEndsAsToday(t *testing.T) {
	f, _ := newLapseFixture(t, healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute), quietCensus)
	unattended(f)
	f.sweepTokens(t)

	if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d; want FAILED, 1", f.st.State(), f.rn.stopCount())
	}
	if !hasEvent(f.audit, f.run.ID, "run.reconcile") {
		t.Error("no run.reconcile row for the ended run")
	}
	if len(f.audit.eventsFor(f.run.ID, "run.lost")) != 0 || len(holdRows(f.audit)) != 0 {
		t.Error("a run ended as today wrote run.lost or a hold row")
	}
}

func TestLostRun_AnOutageKeptRunWhoseCutFailsStaysKeptUnresolved(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	f.lr.proxyErr = errors.New("docker daemon unreachable")
	f.sweepTokens(t)

	if lostAt, _ := f.st.lost(); lostAt == nil || f.st.State() != types.RunRunning {
		t.Fatalf("lost %v, state %s; want RUNNING and lost: a failed cut ends nothing", lostAt, f.st.State())
	}
	if f.st.containmentError() == "" {
		t.Error("containment_error is not set")
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %d, want 1", len(lost))
	}
	d := leaseAuditData(t, lost[0])
	if d["kept"] != true || d["containment"] != "unresolved" || lost[0].Outcome != "failure" {
		t.Errorf("run.lost = %v outcome %q, want kept:true, containment unresolved, outcome failure", d, lost[0].Outcome)
	}
	if f.rn.endCount() != 0 || f.rn.stopCount() != 0 {
		t.Errorf("EndSandbox %d, StopSandbox %d; want 0, 0: no arm ends a run whose cut failed", f.rn.endCount(), f.rn.stopCount())
	}
	if rows := holdRows(f.audit); len(rows) != 0 {
		t.Errorf("hold rows = %d, want 0: held counts only a confirmed cut", len(rows))
	}
}

func TestLostRun_AnOutageKeptRunWhoseEndReadFailsStaysKept(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	f.st.endsErr = errors.New("end read failed")
	f.sweepTokens(t)

	if lostAt, _ := f.st.lost(); lostAt == nil || f.st.State() != types.RunRunning {
		t.Fatalf("lost %v, state %s; want RUNNING and lost", lostAt, f.st.State())
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %d, want 1", len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != true || d["containment"] != "unresolved" {
		t.Errorf("run.lost = %v, want kept:true and containment unresolved", d)
	}
	if f.lr.proxyStopCount() != 1 || f.rn.endCount() != 0 || f.rn.stopCount() != 0 {
		t.Errorf("StopProxy %d, EndSandbox %d, StopSandbox %d; want 1, 0, 0", f.lr.proxyStopCount(), f.rn.endCount(), f.rn.stopCount())
	}
	if rows := holdRows(f.audit); len(rows) != 1 || leaseAuditData(t, rows[0])["held"] != float64(1) {
		t.Errorf("hold rows = %+v, want one with held 1: the cut was confirmed", rows)
	}
}

func TestLostRun_AServerKubernetesRunInAnOutageIsEndedAndTornDown(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	f.lr.proxyErr = runner.ErrEndUnsupported
	f.sweepTokens(t)

	if f.st.State() != types.RunFailed {
		t.Fatalf("state %s, want FAILED", f.st.State())
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %d, want 1", len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != false || d["cause"] != "control_plane_outage" {
		t.Errorf("run.lost data = %v, want kept:false and cause control_plane_outage", d)
	}
	if rows := holdRows(f.audit); len(rows) != 0 {
		t.Errorf("hold rows = %d, want 0: nothing was kept", len(rows))
	}
}

func TestLostRun_ARunnerRunIsNotWidened(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	rid := uuid.New()
	f.st.run.RunnerID = &rid
	f.sweepTokens(t)

	if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d; want FAILED, 1: a headless runner run ends as today", f.st.State(), f.rn.stopCount())
	}
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.lost") {
		if d := leaseAuditData(t, ev); d["kept"] == true {
			t.Errorf("a runner run was kept: %v", d)
		}
	}
	if rows := holdRows(f.audit); len(rows) != 0 {
		t.Errorf("hold rows = %d, want 0", len(rows))
	}
}

// reopen puts the fixture's run back to RUNNING and unlost, as a second run in the same pass would be.
func reopen(f *lostFixture) {
	f.st.mu.Lock()
	f.st.state = types.RunRunning
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.st.mu.Unlock()
	f.srv.leaseEnded.Delete(f.run.ID)
	f.ls.lapsed = true
}

func TestLostRun_OneHoldRowPerEpisode(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	unattended(f)
	f.sweepTokens(t)
	reopen(f)
	f.sweepTokens(t)
	if rows := holdRows(f.audit); len(rows) != 1 {
		t.Fatalf("two outage passes wrote %d hold rows, want 1", len(rows))
	}

	// A healthy pass with no lapsed run closes the episode.
	f.srv.cfg.StoreHealth = healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute)
	f.ls.lapsed = false
	f.sweepTokens(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	reopen(f)
	f.sweepTokens(t)
	if rows := holdRows(f.audit); len(rows) != 2 {
		t.Fatalf("an outage pass after a non-outage pass left %d hold rows, want 2", len(rows))
	}

	// A pass that has a lapsed run and is not outage-class closes the episode too.
	f.srv.cfg.StoreHealth = healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute)
	reopen(f)
	f.sweepTokens(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	reopen(f)
	f.sweepTokens(t)
	if rows := holdRows(f.audit); len(rows) != 3 {
		t.Fatalf("an outage pass after a non-outage pass with a lapsed run left %d hold rows, want 3", len(rows))
	}
}

func TestLostRun_AnInteractiveRunGainsOnlyTheCause(t *testing.T) {
	f, _ := newLapseFixture(t, recoveringTracker(lapseTestGrace), quietCensus)
	f.sweepTokens(t)
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost events = %d, want 1", len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != true || d["cause"] != "control_plane_outage" || d["reason"] != "outage" {
		t.Errorf("run.lost data = %v, want kept:true, reason outage and cause control_plane_outage", d)
	}
	if rows := holdRows(f.audit); len(rows) != 0 {
		t.Errorf("hold rows = %d, want 0: an interactive run is kept as before, not by the widening", len(rows))
	}

	t.Run("no tracker adds no cause", func(t *testing.T) {
		f := newLostFixture(t)
		f.ls.lapsed = true
		f.sweepTokens(t)
		lost := f.audit.eventsFor(f.run.ID, "run.lost")
		if len(lost) != 1 {
			t.Fatalf("run.lost events = %d, want 1", len(lost))
		}
		if _, has := leaseAuditData(t, lost[0])["cause"]; has {
			t.Error("a pass with rule M off wrote a cause")
		}
	})
}
