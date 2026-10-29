// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ store.ServiceRunStore = (*serviceHealthStore)(nil)

// serviceHealthStore is the service-health surface over a map of runs. The embedded Store is nil on
// purpose: a sweep that called TouchRun, or any other store method, would panic and fail its test.
type serviceHealthStore struct {
	store.Store
	mu     sync.Mutex
	runs   map[uuid.UUID]types.AgentRun
	writes map[uuid.UUID]int
	// gone makes SetRunServiceStatus answer false: the run is no longer RUNNING.
	gone bool
}

func (s *serviceHealthStore) ListServiceRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AgentRun
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}

func (s *serviceHealthStore) SetRunServiceStatus(_ context.Context, id uuid.UUID, st types.RunServiceStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return false, nil
	}
	r := s.runs[id]
	r.ServiceStatus = &st
	s.runs[id] = r
	s.writes[id]++
	return true, nil
}

func (s *serviceHealthStore) status(id uuid.UUID) *types.RunServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id].ServiceStatus
}

func (s *serviceHealthStore) writeCount(id uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[id]
}

// serviceStateRunner answers the state file read per sandbox ref and records every exec.
type serviceStateRunner struct {
	*fakeRunner
	mu     sync.Mutex
	answer func(ref string) (*runner.ExecSession, error)
	execs  []runner.ExecSpec
	refs   []string
}

func (r *serviceStateRunner) ExecStream(_ context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	r.execs, r.refs = append(r.execs, spec), append(r.refs, ref)
	r.mu.Unlock()
	return r.answer(ref)
}

func (r *serviceStateRunner) readRefs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	refs := slices.Clone(r.refs)
	slices.Sort(refs)
	return refs
}

type serviceHealthFixture struct {
	srv   *Server
	st    *serviceHealthStore
	rn    *serviceStateRunner
	audit *recRecorder
	now   time.Time
}

func newServiceHealthFixture(t *testing.T, answer func(ref string) (*runner.ExecSession, error), cfgFn func(*Config), runs ...types.AgentRun) *serviceHealthFixture {
	t.Helper()
	f := &serviceHealthFixture{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f.st = &serviceHealthStore{runs: map[uuid.UUID]types.AgentRun{}, writes: map[uuid.UUID]int{}}
	for _, r := range runs {
		f.st.runs[r.ID] = r
	}
	f.rn = &serviceStateRunner{fakeRunner: &fakeRunner{}, answer: answer}
	h := newHarness(t)
	f.audit = h.audit
	cfg := baseTestConfig(h, f.st)
	cfg.Runner = f.rn
	cfg.Now = func() time.Time { return f.now }
	if cfgFn != nil {
		cfgFn(&cfg)
	}
	f.srv = New(cfg)
	return f
}

func (f *serviceHealthFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepServiceHealth(context.Background()); err != nil {
		t.Fatalf("sweepServiceHealth: %v", err)
	}
}

func (f *serviceHealthFixture) auditCount(runID uuid.UUID, action string) int {
	n := 0
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action && ev.RunID != nil && *ev.RunID == runID {
			n++
		}
	}
	return n
}

// serviceRun is a RUNNING service run created age ago, in sandbox ref.
func serviceRun(now time.Time, ref string, age time.Duration) types.AgentRun {
	return types.AgentRun{
		ID: uuid.New(), State: types.RunRunning, SandboxRef: ref, CreatedAt: now.Add(-age),
		Service: &types.RunService{Restart: types.ServiceRestartOnFailure},
	}
}

func stateFileJSON(health, reason string, restarts int, exit string) string {
	return fmt.Sprintf(`{"v":1,"health":%q,"reason":%q,"restarts":%d,"last_exit_code":%s}`+"\n", health, reason, restarts, exit)
}

func answerWith(text string, code int) func(string) (*runner.ExecSession, error) {
	pane := textPane(text, code)
	return func(string) (*runner.ExecSession, error) { return pane(runner.ExecSpec{}) }
}

func TestDecodeServiceState(t *testing.T) {
	exit143 := 143
	good := []struct {
		name string
		in   string
		want serviceStateFile
	}{
		{"starting", `{"v":1,"health":"starting","reason":"","restarts":0,"last_exit_code":null}`,
			serviceStateFile{V: 1, Health: "starting"}},
		{"restarting", stateFileJSON("unhealthy", "restarting", 4, "143"),
			serviceStateFile{V: 1, Health: "unhealthy", Reason: "restarting", Restarts: 4, LastExitCode: &exit143}},
		{"probe failing", stateFileJSON("unhealthy", "probe_failing", 0, "null"),
			serviceStateFile{V: 1, Health: "unhealthy", Reason: "probe_failing"}},
		{"reason absent", `{"v":1,"health":"healthy","restarts":1,"last_exit_code":0}`,
			serviceStateFile{V: 1, Health: "healthy", Restarts: 1, LastExitCode: new(int)}},
	}
	for _, c := range good {
		got, err := decodeServiceState([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !sameServiceStatus(types.RunServiceStatus{Health: types.ServiceHealthState(got.Health), Reason: got.Reason, Restarts: got.Restarts, LastExitCode: got.LastExitCode},
			types.RunServiceStatus{Health: types.ServiceHealthState(c.want.Health), Reason: c.want.Reason, Restarts: c.want.Restarts, LastExitCode: c.want.LastExitCode}) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	for name, in := range map[string]string{
		"empty":            ``,
		"garbage":          `not json`,
		"array":            `[1]`,
		"no version":       `{"health":"healthy","reason":"","restarts":0,"last_exit_code":null}`,
		"future version":   `{"v":2,"health":"healthy","reason":"","restarts":0,"last_exit_code":null}`,
		"string version":   `{"v":"1","health":"healthy","reason":"","restarts":0,"last_exit_code":null}`,
		"unknown key":      `{"v":1,"health":"healthy","reason":"","restarts":0,"last_exit_code":null,"extra":1}`,
		"health unknown":   stateFileJSON("unknown", "", 0, "null"),
		"health empty":     stateFileJSON("", "", 0, "null"),
		"health case":      stateFileJSON("Healthy", "", 0, "null"),
		"reason invented":  stateFileJSON("unhealthy", "unreachable", 0, "null"),
		"fractional count": `{"v":1,"health":"healthy","reason":"","restarts":1.5,"last_exit_code":null}`,
		"count as string":  `{"v":1,"health":"healthy","reason":"","restarts":"1","last_exit_code":null}`,
		"two objects":      stateFileJSON("healthy", "", 0, "null") + stateFileJSON("healthy", "", 0, "null"),
		"trailing junk":    stateFileJSON("healthy", "", 0, "null") + "x",
	} {
		if got, err := decodeServiceState([]byte(in)); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, got)
		}
	}
	big, err := decodeServiceState([]byte(stateFileJSON("healthy", "", 99999999, "9999")))
	if err != nil || big.Restarts != serviceRestartsMax || big.LastExitCode == nil || *big.LastExitCode != serviceExitMax {
		t.Errorf("clamp: %+v, %v", big, err)
	}
	neg, err := decodeServiceState([]byte(stateFileJSON("healthy", "", -3, "-1")))
	if err != nil || neg.Restarts != 0 || neg.LastExitCode == nil || *neg.LastExitCode != 0 {
		t.Errorf("clamp negatives: %+v, %v", neg, err)
	}
}

// TestServiceHealthSweep_StatesFromTheSandbox: every way the read can come out, and the status it stores.
func TestServiceHealthSweep_StatesFromTheSandbox(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	exit137 := 137
	admittedRecently := serviceRun(now, "ref", time.Hour)
	admitted := now.Add(-time.Minute)
	admittedRecently.AdmittedAt = &admitted
	cases := []struct {
		name   string
		run    types.AgentRun
		answer func(string) (*runner.ExecSession, error)
		health types.ServiceHealthState
		reason string
		counts int
		exit   *int
	}{
		{"healthy", serviceRun(now, "ref", time.Hour), answerWith(stateFileJSON("healthy", "", 2, "137"), 0),
			types.ServiceHealthHealthy, "", 2, &exit137},
		{"restarting", serviceRun(now, "ref", time.Hour), answerWith(stateFileJSON("unhealthy", "restarting", 3, "137"), 0),
			types.ServiceHealthUnhealthy, "restarting", 3, &exit137},
		{"garbage", serviceRun(now, "ref", time.Hour), answerWith("rm -rf /\n", 0),
			types.ServiceHealthUnknown, "unreachable", 0, nil},
		{"empty", serviceRun(now, "ref", time.Hour), answerWith("", 0),
			types.ServiceHealthUnknown, "unreachable", 0, nil},
		{"oversize", serviceRun(now, "ref", time.Hour), answerWith(strings.Repeat("x", serviceStateMaxBytes+1), 0),
			types.ServiceHealthUnknown, "unreachable", 0, nil},
		{"exec error", serviceRun(now, "ref", time.Hour),
			func(string) (*runner.ExecSession, error) { return nil, errors.New("runner: the runner is offline") },
			types.ServiceHealthUnknown, "unreachable", 0, nil},
		{"sandbox gone", serviceRun(now, "ref", time.Hour),
			func(string) (*runner.ExecSession, error) { return nil, runner.ErrSandboxGone },
			types.ServiceHealthUnknown, "unreachable", 0, nil},
		{"missing early", serviceRun(now, "ref", time.Minute), answerWith("", 1),
			types.ServiceHealthStarting, "", 0, nil},
		{"missing late", serviceRun(now, "ref", 11*time.Minute), answerWith("", 1),
			types.ServiceHealthUnknown, "supervisor_missing", 0, nil},
		{"missing, admitted recently", admittedRecently, answerWith("", 1),
			types.ServiceHealthStarting, "", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceHealthFixture(t, c.answer, nil, c.run)
			f.sweep(t)
			got := f.st.status(c.run.ID)
			if got == nil {
				t.Fatal("no status stored")
			}
			if got.Health != c.health || got.Reason != c.reason || got.Restarts != c.counts ||
				(got.LastExitCode == nil) != (c.exit == nil) || (c.exit != nil && *got.LastExitCode != *c.exit) {
				t.Errorf("status = %+v, want %s/%s restarts %d exit %v", *got, c.health, c.reason, c.counts, c.exit)
			}
			if !got.ObservedAt.Equal(f.now) {
				t.Errorf("observed_at = %v, want wardynd's clock %v", got.ObservedAt, f.now)
			}
			if len(f.rn.execs) != 1 {
				t.Fatalf("execs = %d, want 1", len(f.rn.execs))
			}
			spec := f.rn.execs[0]
			if !slices.Equal(spec.Argv, []string{"cat", "/home/agent/.wardyn/service.json"}) || len(spec.Env) != 0 || spec.TTY {
				t.Errorf("exec = %+v, want the fixed cat with no env and no TTY", spec)
			}
		})
	}
}

// A read that outlives its bound is unreachable, and its exec is torn down.
func TestReadServiceState_TimeoutIsUnreachable(t *testing.T) {
	pr, pw := io.Pipe()
	closed := make(chan struct{})
	f := newServiceHealthFixture(t, func(string) (*runner.ExecSession, error) {
		return &runner.ExecSession{
			Stdout: pr, Stderr: strings.NewReader(""),
			Wait:  func() (int, error) { return 0, nil },
			Close: func() error { _ = pw.Close(); close(closed); return nil },
		}, nil
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, probe := f.srv.readServiceState(ctx, "ref")
	if probe != serviceProbeUnreachable {
		t.Fatalf("probe = %v, want unreachable", probe)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the timed-out exec was not closed")
	}
}

// TestServiceHealthSweep_AuditsOnlyTransitions: a steady service writes no audit rows however often it
// is read, and a stored refresh every five minutes is not a transition.
func TestServiceHealthSweep_AuditsOnlyTransitions(t *testing.T) {
	var body string
	var mu sync.Mutex
	set := func(s string) { mu.Lock(); body = s; mu.Unlock() }
	answer := func(string) (*runner.ExecSession, error) {
		mu.Lock()
		defer mu.Unlock()
		return textPane(body, 0)(runner.ExecSpec{})
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := serviceRun(now, "ref", time.Hour)
	f := newServiceHealthFixture(t, answer, nil, run)
	rows := func() (health, restart int) {
		return f.auditCount(run.ID, "run.service.health"), f.auditCount(run.ID, "run.service.restart")
	}
	step := func(name, text string, advance time.Duration, wantWrites, wantHealth, wantRestart int) {
		t.Helper()
		set(text)
		f.now = f.now.Add(advance)
		f.sweep(t)
		h, r := rows()
		if w := f.st.writeCount(run.ID); w != wantWrites || h != wantHealth || r != wantRestart {
			t.Fatalf("%s: writes %d health rows %d restart rows %d; want %d, %d, %d", name, w, h, r, wantWrites, wantHealth, wantRestart)
		}
	}
	step("first read: starting is the baseline", stateFileJSON("starting", "", 0, "null"), 0, 1, 0, 0)
	step("starting to healthy", stateFileJSON("healthy", "", 0, "null"), time.Minute, 2, 1, 0)
	step("unchanged a minute later", stateFileJSON("healthy", "", 0, "null"), time.Minute, 2, 1, 0)
	step("unchanged, refreshed after five minutes", stateFileJSON("healthy", "", 0, "null"), 6*time.Minute, 3, 1, 0)
	step("crash", stateFileJSON("unhealthy", "restarting", 1, "137"), time.Minute, 4, 2, 1)
	step("reason moves, health does not", stateFileJSON("unhealthy", "probe_failing", 1, "137"), time.Minute, 5, 2, 1)
	step("two more restarts, still unhealthy", stateFileJSON("unhealthy", "restarting", 3, "1"), time.Minute, 6, 2, 2)

	events := f.audit.snapshot()
	restart := findAudit(events, run.ID, "run.service.restart", "success")
	for _, want := range []string{`"restarts":3`, `"delta":2`, `"last_exit_code":1`, `"source":"sandbox"`} {
		if restart == nil || !strings.Contains(string(restart.Data), want) {
			t.Errorf("restart row = %+v, want %s", restart, want)
		}
	}
	health := findAudit(events, run.ID, "run.service.health", "success")
	for _, want := range []string{`"from":"healthy"`, `"to":"unhealthy"`, `"reason":"restarting"`, `"source":"sandbox"`} {
		if health == nil || !strings.Contains(string(health.Data), want) {
			t.Errorf("health row = %+v, want %s", health, want)
		}
	}
	if health != nil && (health.ActorType != types.ActorSystem || health.Actor != "wardynd") {
		t.Errorf("health row actor = %s/%s, want system/wardynd", health.ActorType, health.Actor)
	}
}

// A blip keeps the counts, so the recovery does not read as a fresh run of restarts.
func TestServiceHealthSweep_UnreachableKeepsTheLastCounts(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := serviceRun(now, "ref", time.Hour)
	exit := 1
	run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, Restarts: 5, LastExitCode: &exit, ObservedAt: now.Add(-time.Minute)}
	f := newServiceHealthFixture(t, func(string) (*runner.ExecSession, error) { return nil, errors.New("down") }, nil, run)
	f.sweep(t)
	got := f.st.status(run.ID)
	if got == nil || got.Health != types.ServiceHealthUnknown || got.Restarts != 5 || got.LastExitCode == nil || *got.LastExitCode != 1 {
		t.Fatalf("status = %+v, want unknown with 5 restarts and exit 1", got)
	}
	if f.auditCount(run.ID, "run.service.health") != 1 || f.auditCount(run.ID, "run.service.restart") != 0 {
		t.Errorf("rows = %d health, %d restart; want 1, 0", f.auditCount(run.ID, "run.service.health"), f.auditCount(run.ID, "run.service.restart"))
	}
}

// A status the store refused to write (the run ended meanwhile) earns no audit row.
func TestServiceHealthSweep_UnwrittenStatusIsNotAudited(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := serviceRun(now, "ref", time.Hour)
	f := newServiceHealthFixture(t, answerWith(stateFileJSON("unhealthy", "restarting", 2, "1"), 0), nil, run)
	f.st.gone = true
	f.sweep(t)
	if n := len(f.audit.snapshot()); n != 0 {
		t.Fatalf("audit rows = %d, want 0", n)
	}
}

// TestServiceHealthSweep_LeaderAndHolderSplit: a run with no runner is the leader's alone; a run with a
// runner is read by whichever replica holds that runner's session, leader or not; a runner held
// elsewhere is not read at all.
func TestServiceHealthSweep_LeaderAndHolderSplit(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	heldID, otherID := uuid.New(), uuid.New()
	local := serviceRun(now, "ref-local", time.Hour)
	held := serviceRun(now, "ref-held", time.Hour)
	held.RunnerID = &heldID
	elsewhere := serviceRun(now, "ref-elsewhere", time.Hour)
	elsewhere.RunnerID = &otherID
	for _, c := range []struct {
		name   string
		lease  *fakeLease
		wantFn []string
	}{
		{"follower", &fakeLease{}, []string{"ref-held"}},
		{"leader", &fakeLease{lead: true, termCtx: context.Background(), current: func(int64) bool { return true }}, []string{"ref-held", "ref-local"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceHealthFixture(t, answerWith(stateFileJSON("healthy", "", 0, "null"), 0),
				func(cfg *Config) {
					cfg.SweeperLease = c.lease
					cfg.RunnerOnline = func(id uuid.UUID) bool { return id == heldID }
				}, local, held, elsewhere)
			f.sweep(t)
			if got := f.rn.readRefs(); !slices.Equal(got, c.wantFn) {
				t.Errorf("read %v, want %v", got, c.wantFn)
			}
			if f.st.status(elsewhere.ID) != nil {
				t.Errorf("a runner held elsewhere was written")
			}
			if begins, ends := c.lease.counts(); begins != ends {
				t.Errorf("lease begins %d ends %d: the term was not released", begins, ends)
			}
		})
	}
}

// With no SweeperLease the sweep runs as it always did, one replica reading everything it can reach.
func TestServiceHealthSweep_NoLeaseReadsLocalRuns(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := serviceRun(now, "ref", time.Hour)
	f := newServiceHealthFixture(t, answerWith(stateFileJSON("healthy", "", 0, "null"), 0), nil, run)
	f.sweep(t)
	if s := f.st.status(run.ID); s == nil || s.Health != types.ServiceHealthHealthy {
		t.Fatalf("status = %+v, want healthy", s)
	}
}

// A store with no service surface is a no-op, not an error.
func TestServiceHealthSweep_StoreWithoutTheSurface(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &dispatchTestStore{})
	cfg.Runner = &serviceStateRunner{fakeRunner: &fakeRunner{}, answer: func(string) (*runner.ExecSession, error) {
		t.Fatal("read with no store surface")
		return nil, nil
	}}
	if err := New(cfg).sweepServiceHealth(context.Background()); err != nil {
		t.Fatalf("sweepServiceHealth: %v", err)
	}
}
