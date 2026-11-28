// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// editStateRun rewrites one run's object in the state file under dir, between a Close and a New, as an
// older or hand-edited runnerd would have left it.
func editStateRun(t *testing.T, dir string, id uuid.UUID, edit func(run map[string]json.RawMessage)) {
	t.Helper()
	path := filepath.Join(dir, "offline", "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Runs map[string]map[string]json.RawMessage `json:"runs"`
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || json.Unmarshal(raw, &st) != nil {
		t.Fatal("state file does not parse")
	}
	run, ok := st.Runs[id.String()]
	if !ok {
		t.Fatal("state file has no such run")
	}
	edit(run)
	top["runs"], _ = json.Marshal(st.Runs)
	edited, _ := json.Marshal(top)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
}

// dropStateLapseBy removes lapse_by from id's saved run, as an older runnerd wrote it.
func dropStateLapseBy(t *testing.T, dir string, id uuid.UUID) {
	t.Helper()
	editStateRun(t, dir, id, func(run map[string]json.RawMessage) {
		if _, ok := run["lapse_by"]; !ok {
			t.Fatal("state file has no lapse_by to drop")
		}
		delete(run, "lapse_by")
	})
}

// setStateLapseBy sets id's saved lapse_by to at.
func setStateLapseBy(t *testing.T, dir string, id uuid.UUID, at time.Time) {
	t.Helper()
	editStateRun(t, dir, id, func(run map[string]json.RawMessage) {
		b, err := json.Marshal(at)
		if err != nil {
			t.Fatal(err)
		}
		run["lapse_by"] = b
	})
}

// restartDocker closes m and reloads its state on the same fake clock with a substrate that does not lapse
// its proxies itself, as a Docker runner restarts, then gives held runs their provisional deadlines. before,
// when non-nil, runs between the close and the reload.
func restartDocker(t *testing.T, m *Manager, sub *localSub, elapsed *time.Duration, base time.Time, before func()) *Manager {
	t.Helper()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before()
	}
	restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.now = func() time.Time { return base.Add(*elapsed) }
	restarted.since = func(at time.Time) time.Duration { return base.Add(*elapsed).Sub(at) }
	restarted.clock = NewClock(restarted.since)
	restarted.ProvisionalDeadlines()
	return restarted
}

func TestDockerRestartKeepsAProxyUntilItsEndsAt(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(20 * time.Minute)})
	*elapsed = 10 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, nil)
	expectStops(t, restarted, sub, 0)
	*elapsed = 19 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 21 * time.Minute
	expectStops(t, restarted, sub, 1)
}

func TestDockerRestartKeepsTheTokenLeg(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	// The token leg is RunTokenLapseAfter (1h5m) less the 40m since the renewal: 25m.
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(-40 * time.Minute), EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 10 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, nil)
	expectStops(t, restarted, sub, 0)
	*elapsed = 24 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 26 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// TestDockerRestartPastItsLapseByContainsAtOnce pins that Docker leaves no grace: the k8s arm gives such a run
// restartReadyGrace more (TestProvisionalDeadlinePassedLapseByGetsReadyGrace).
func TestDockerRestartPastItsLapseByContainsAtOnce(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(20 * time.Minute)})
	*elapsed = 30 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, nil)
	expectStops(t, restarted, sub, 1)
}

func TestDockerRestartWithoutLapseByContainsAtOnce(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 10 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, func() { dropStateLapseBy(t, m.dir, id) })
	expectStops(t, restarted, sub, 1)
}

func TestDockerRestartLoopStillContainsAtItsLapseBy(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(20 * time.Minute)})
	current := m
	for _, at := range []time.Duration{5, 10, 15, 19} {
		*elapsed = at * time.Minute
		current = restartDocker(t, current, sub, elapsed, base, nil)
		expectStops(t, current, sub, 0)
	}
	*elapsed = 21 * time.Minute
	current = restartDocker(t, current, sub, elapsed, base, nil)
	expectStops(t, current, sub, 1)
}

// TestDockerRestartDeadlineAdmitsNoExecutionUntilAFreshLease pins cleanupOnly: the deadline defers the proxy's
// containment and nothing else.
func TestDockerRestartDeadlineAdmitsNoExecutionUntilAFreshLease(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 10 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, nil)
	ctx := context.Background()
	if err := restarted.Clock(runnerwire.Hello{ServerTime: org.Add(10 * time.Minute)}, restarted.now()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BeforeReady(ctx, &runnerwire.Peer{}, false); err != nil {
		t.Fatalf("BeforeReady: %v", err)
	}
	if sub.stops != 0 {
		t.Fatalf("stops=%d after READY, want the proxy kept until its saved lapse time", sub.stops)
	}
	if got := restarted.AttachRef(id); got != "" {
		t.Fatalf("AttachRef = %q on a provisional deadline, want none", got)
	}
	if err := restarted.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("replace_proxy on a provisional deadline = %v, want ErrRunnerOffline", err)
	}
	if err := restarted.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(10 * time.Minute), EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatalf("fresh lease: %v", err)
	}
	if got := restarted.AttachRef(id); got != id.String() {
		t.Fatalf("AttachRef = %q after a fresh lease, want %q", got, id.String())
	}
}

func TestDockerRestartContainsWhatItCannotVouchFor(t *testing.T) {
	g := uuid.New()
	rows := []struct {
		name  string
		setup func(t *testing.T, m *Manager, org time.Time) uuid.UUID
	}{
		{"create not finished", func(t *testing.T, m *Manager, org time.Time) uuid.UUID {
			id := uuid.New()
			if err := m.Lease(context.Background(), runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := m.BeforeCreate(context.Background(), runner.SandboxSpec{RunID: id}); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"lease superseded", edited(func(r *localRun) { r.LeaseAfter = r.Lease.TokenRenewedAt })},
		{"containment marker", edited(func(r *localRun) {
			r.Containment = &types.RunnerMutationFailure{ID: uuid.New(), Operation: "stop_proxy", Contained: true}
		})},
		{"lost execution result", edited(func(r *localRun) {
			r.Generation, r.Executions, r.ExecID = g, map[uuid.UUID]string{g: "exec"}, ""
		})},
		{"containment pending", edited(func(r *localRun) { r.ContainmentPending = &runnerwire.ContainmentPending{} })},
		{"teardown unconfirmed", edited(func(r *localRun) { r.TeardownUnconfirmed = &teardownHold{} })},
		{"quarantined", edited(func(r *localRun) { r.Quarantine = &types.RunnerQuarantine{} })},
		{"state from an earlier runner", edited(func(r *localRun) { r.CreateDone = false })},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			m, sub, elapsed, org := managerFixture(t)
			base := m.now()
			row.setup(t, m, org)
			*elapsed = 10 * time.Minute
			restarted := restartDocker(t, m, sub, elapsed, base, nil)
			expectStops(t, restarted, sub, 1)
		})
	}
}

// edited builds a finished run with a 3h end, applies edit to its saved state and saves it.
func edited(edit func(r *localRun)) func(t *testing.T, m *Manager, org time.Time) uuid.UUID {
	return func(t *testing.T, m *Manager, org time.Time) uuid.UUID {
		t.Helper()
		id := createLocal(t, m, org)
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.state.Runs[id]
		edit(&r)
		m.state.Runs[id] = r
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
		return id
	}
}

// TestDockerRestartDeadlineIsAtMostOneLapseWindow pins the RunTokenLapseAfter cap on a wall-clock LapseBy
// (a host clock stepped back before the restart).
func TestDockerRestartDeadlineIsAtMostOneLapseWindow(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 10 * time.Minute
	restarted := restartDocker(t, m, sub, elapsed, base, func() {
		setStateLapseBy(t, m.dir, id, base.Add(10*time.Minute+5*time.Hour))
	})
	*elapsed = 10*time.Minute + 64*time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 10*time.Minute + 66*time.Minute
	expectStops(t, restarted, sub, 1)
}

func TestCreatedRecordsCreateDone(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if !m.state.Runs[id].CreateDone {
		t.Fatal("Created left CreateDone false")
	}
	other := uuid.New()
	if err := m.Lease(context.Background(), runnerwire.Lease{RunID: other, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeCreate(context.Background(), runner.SandboxSpec{RunID: other}); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[other].CreateDone {
		t.Fatal("an interrupted create reads as finished")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	if !reloaded.state.Runs[id].CreateDone || reloaded.state.Runs[other].CreateDone {
		t.Fatal("CreateDone did not survive a restart as saved")
	}
}
