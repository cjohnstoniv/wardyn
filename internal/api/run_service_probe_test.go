// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portCheckRunner answers each exec by its argv, so a test gives the state file and the port check their own answers.
type portCheckRunner struct {
	*fakeRunner
	mu     sync.Mutex
	answer func(spec runner.ExecSpec) (*runner.ExecSession, error)
	execs  [][]string
}

func (r *portCheckRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	r.execs = append(r.execs, slices.Clone(spec.Argv))
	r.mu.Unlock()
	return r.answer(spec)
}

func (r *portCheckRunner) argvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.execs)
}

// writeLog is a stdin that keeps what was written to it.
type writeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *writeLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *writeLog) Close() error { return nil }
func (w *writeLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// portAnswer is an exec whose stdout is out and whose exit code is code.
func portAnswer(out string, code int, stdin io.WriteCloser) *runner.ExecSession {
	return &runner.ExecSession{
		Stdin: stdin, Stdout: strings.NewReader(out), Stderr: strings.NewReader(""),
		Wait:  func() (int, error) { return code, nil },
		Close: func() error { return nil },
	}
}

func probedService(path string) *types.RunService {
	return &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080,
		Health: &types.ServiceHealth{Path: path, IntervalSec: 15, FailureThreshold: 3, Probe: types.ServiceProbeControlPlane}}
}

// The check sends one GET of the path to the declared port and nothing else: no credentials, no cookie, and the
// connection closed after the answer. Without a path it connects and sends nothing. Both are socat on the
// sandbox's own loopback, the relay the UI gateway uses.
func TestServiceCheckArgvAndRequest(t *testing.T) {
	http := probedService("/healthz")
	if got := serviceCheckArgv(http); !slices.Equal(got, []string{"socat", "-", "TCP:127.0.0.1:8080"}) {
		t.Errorf("HTTP argv = %q", got)
	}
	want := "GET /healthz HTTP/1.0\r\nHost: 127.0.0.1:8080\r\nUser-Agent: wardyn-health\r\nConnection: close\r\n\r\n"
	if got := string(serviceCheckRequest(http)); got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	tcp := probedService("")
	if got := serviceCheckArgv(tcp); !slices.Equal(got, []string{"socat", "-u", "/dev/null", "TCP:127.0.0.1:8080"}) {
		t.Errorf("TCP argv = %q", got)
	}
	if got := serviceCheckRequest(tcp); got != nil {
		t.Errorf("a TCP connect sends %q, want nothing", got)
	}
}

// The status line is parsed strictly: HTTP/1.x, three digits, a printable reason, the line's end. 2xx and 3xx
// pass; any other status is bad_status with its code; anything else is bad_answer.
func TestJudgeServiceStatusLine(t *testing.T) {
	for _, tc := range []struct {
		in      string
		finding string
		status  int
	}{
		{"HTTP/1.1 200 OK\r\n", "", 200},
		{"HTTP/1.0 204 No Content\r\n", "", 204},
		{"HTTP/1.1 301 Moved Permanently\r\nLocation: http://evil.example/\r\n", "", 301},
		{"HTTP/1.1 399\r\n", "", 399},
		{"HTTP/1.1 200 OK\n", "", 200},
		{"HTTP/1.1 400 Bad Request\r\n", "bad_status", 400},
		{"HTTP/1.1 503 Service Unavailable\r\n", "bad_status", 503},
		{"HTTP/1.1 101 Switching Protocols\r\n", "bad_status", 101},
		{"HTTP/1.1 600 Nope\r\n", "bad_answer", 0},
		{"HTTP/2 200\r\n", "bad_answer", 0},
		{"http/1.1 200 OK\r\n", "bad_answer", 0},
		{"HTTP/1.1 20 OK\r\n", "bad_answer", 0},
		{"HTTP/1.1 2000 OK\r\n", "bad_answer", 0},
		{"HTTP/1.1 200OK\r\n", "bad_answer", 0},
		{"HTTP/1.1 200 OK\x00\r\n", "bad_answer", 0},
		{" HTTP/1.1 200 OK\r\n", "bad_answer", 0},
		{"SSH-2.0-OpenSSH_9.6\r\n", "bad_answer", 0},
		{"{\"health\":\"healthy\"}\n", "bad_answer", 0},
	} {
		got := judgeServiceStatusLine([]byte(tc.in))
		if got.finding != tc.finding || got.status != tc.status {
			t.Errorf("%q = %+v, want finding %q status %d", tc.in, got, tc.finding, tc.status)
		}
	}
}

// What comes back from the exec, judged: the answer's first line for an HTTP check, the exit code when there is
// no answer, and the bound on how much is read.
func TestServiceCheckAnswer(t *testing.T) {
	big := strings.Repeat("x", serviceCheckMaxBytes+10) + "\r\n"
	for _, tc := range []struct {
		name    string
		path    string
		out     string
		code    int
		finding string
		status  int
	}{
		{"200", "/healthz", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", 0, "", 200},
		{"a redirect passes and is not followed", "/healthz", "HTTP/1.1 302 Found\r\nLocation: /login\r\n\r\n", 0, "", 302},
		{"503", "/healthz", "HTTP/1.1 503 Service Unavailable\r\n\r\n", 0, "bad_status", 503},
		{"garbage", "/healthz", "hello there\n", 0, "bad_answer", 0},
		{"oversize with no line", "/healthz", big, 0, "bad_answer", 0},
		{"a line cut off by the exec's end", "/healthz", "HTTP/1.1 200 OK", 0, "bad_answer", 0},
		{"accepted, then closed with nothing", "/healthz", "", 0, "bad_answer", 0},
		{"nothing listening", "/healthz", "", 1, "refused", 0},
		{"no socat in the image", "/healthz", "", 127, "not_run", 0},
		{"socat not executable", "/healthz", "", 126, "not_run", 0},
		{"TCP: accepted", "", "", 0, "", 0},
		{"TCP: nothing listening", "", "", 1, "refused", 0},
		{"TCP: no socat", "", "", 127, "not_run", 0},
		{"TCP: unexpected bytes", "", "SSH-2.0-x\r\n", 0, "bad_answer", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin := &writeLog{}
			sv := probedService(tc.path)
			got := serviceCheckAnswer(portAnswer(tc.out, tc.code, stdin), serviceCheckRequest(sv))
			if got.finding != tc.finding || got.status != tc.status {
				t.Fatalf("got %+v, want finding %q status %d", got, tc.finding, tc.status)
			}
			if want := string(serviceCheckRequest(sv)); stdin.String() != want {
				t.Errorf("stdin = %q, want %q", stdin.String(), want)
			}
		})
	}
}

// A check that cannot start is not_run, never a failure of the service; one with no answer inside its time is a
// timeout, and the exec is closed either way.
func TestCheckServicePort_NotRunAndTimeout(t *testing.T) {
	run := serviceRun(time.Now(), "ref", time.Minute)
	run.Service = probedService("/healthz")
	f := newServiceHealthFixture(t, nil, func(c *Config) {
		c.Runner = &portCheckRunner{fakeRunner: &fakeRunner{}, answer: func(runner.ExecSpec) (*runner.ExecSession, error) {
			return nil, errors.New("exec refused")
		}}
	})
	if got := f.srv.checkServicePort(context.Background(), run); got.finding != serviceCheckNotRun {
		t.Fatalf("exec error = %+v, want not_run", got)
	}

	pr, pw := io.Pipe()
	closed := make(chan struct{})
	f = newServiceHealthFixture(t, nil, func(c *Config) {
		c.Runner = &portCheckRunner{fakeRunner: &fakeRunner{}, answer: func(runner.ExecSpec) (*runner.ExecSession, error) {
			return &runner.ExecSession{
				Stdin: &writeLog{}, Stdout: pr, Stderr: strings.NewReader(""),
				Wait:  func() (int, error) { return 0, nil },
				Close: func() error { _ = pw.Close(); close(closed); return nil },
			}, nil
		}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := f.srv.checkServicePort(ctx, run); got.finding != serviceCheckTimedOut {
		t.Fatalf("no answer = %+v, want timeout", got)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the timed-out exec was not closed")
	}
}

// Failures count Wardyn's own checks: a failure below the threshold keeps the last health (starting before any)
// and counts; the threshold reads unhealthy; a pass resets; a check that could not run reads unknown/not_run and
// counts nothing; a pass with no check keeps the last reading as it was.
func TestServiceProbeObservation_Counts(t *testing.T) {
	t0 := time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
	fail := &serviceCheck{finding: serviceCheckBadStatus, status: 503}
	pass := &serviceCheck{status: 200}
	notRun := &serviceCheck{finding: serviceCheckNotRun}
	type step struct {
		check    *serviceCheck
		health   types.ServiceHealthState
		reason   string
		failures int
	}
	steps := []step{
		{fail, types.ServiceHealthStarting, "bad_status", 1},
		{fail, types.ServiceHealthStarting, "bad_status", 2},
		{fail, types.ServiceHealthUnhealthy, "bad_status", 3},
		{fail, types.ServiceHealthUnhealthy, "bad_status", 4},
		{pass, types.ServiceHealthHealthy, "", 0},
		{&serviceCheck{finding: serviceCheckTimedOut}, types.ServiceHealthHealthy, "timeout", 1},
		{notRun, types.ServiceHealthUnknown, "not_run", 1},
		{&serviceCheck{finding: serviceCheckRefused}, types.ServiceHealthUnknown, "refused", 2},
		{&serviceCheck{finding: serviceCheckBadAnswer}, types.ServiceHealthUnhealthy, "bad_answer", 3},
		{pass, types.ServiceHealthHealthy, "", 0},
	}
	var prev *types.RunServiceProbe
	for i, s := range steps {
		now := t0.Add(time.Duration(i) * time.Minute)
		got := serviceProbeObservation(prev, s.check, 3, now)
		if got.Health != s.health || got.Reason != s.reason || got.Failures != s.failures || !got.ObservedAt.Equal(now) {
			t.Fatalf("step %d: %+v, want %s/%q failures %d", i, *got, s.health, s.reason, s.failures)
		}
		prev = got
	}
	if prev.HTTPStatus != 200 {
		t.Errorf("a pass keeps its status: %+v", *prev)
	}
	kept := serviceProbeObservation(prev, nil, 3, t0.Add(time.Hour))
	if kept == prev || !kept.ObservedAt.Equal(prev.ObservedAt) || !sameServiceProbe(kept, prev) {
		t.Errorf("no check this pass = %+v, want a copy of %+v", kept, prev)
	}
	if serviceProbeObservation(nil, nil, 3, t0) != nil {
		t.Error("no check and no reading made a reading")
	}
	if got := serviceProbeObservation(nil, fail, 1, t0); got.Health != types.ServiceHealthUnhealthy {
		t.Errorf("threshold 1: %+v, want unhealthy at the first failure", *got)
	}
}

// probeSweepFixture is a health fixture whose runner answers the state file with stateFile and the port check
// with port(); run is a fresh service that opted in to Wardyn's check.
func probeSweepFixture(t *testing.T, stateFile string, port func() (string, int), sv *types.RunService) (*serviceHealthFixture, *portCheckRunner, types.AgentRun) {
	t.Helper()
	pr := &portCheckRunner{fakeRunner: &fakeRunner{}, answer: func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] == "cat" {
			return portAnswer(stateFile, 0, nil), nil
		}
		out, code := port()
		return portAnswer(out, code, &writeLog{}), nil
	}}
	now := time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
	run := serviceRun(now, "ref", time.Minute)
	run.Service = sv
	f := newServiceHealthFixture(t, nil, func(c *Config) { c.Runner = pr }, run)
	return f, pr, run
}

func (f *serviceHealthFixture) healthRows(t *testing.T, source string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range f.audit.snapshot() {
		if ev.Action != "run.service.health" {
			continue
		}
		if d := leaseAuditData(t, ev); d["source"] == source {
			out = append(out, d)
		}
	}
	return out
}

// With the check off, a pass runs the one state-file read and stores no probe: the path is the one it was.
func TestServiceHealthSweep_ProbeOffRunsOnlyTheFileRead(t *testing.T) {
	off := probedService("/healthz")
	off.Health.Probe = ""
	f, pr, run := probeSweepFixture(t, stateFileJSON("healthy", "", 0, "null"), func() (string, int) {
		t.Fatal("a service with the check off was checked")
		return "", 0
	}, off)
	f.sweep(t)
	if got := pr.argvs(); len(got) != 1 || got[0][0] != "cat" {
		t.Fatalf("execs = %q, want the state-file read alone", got)
	}
	st := f.st.status(run.ID)
	if st == nil || st.Health != types.ServiceHealthHealthy || st.Probe != nil {
		t.Fatalf("status = %+v, want healthy with no probe", st)
	}
	if rows := f.healthRows(t, "control_plane"); len(rows) != 0 {
		t.Errorf("control_plane rows = %v, want none", rows)
	}
}

// With the check on, each pass reads the file, then checks the port; failures count passes of Wardyn's own check
// (not the supervisor's samples), the threshold turns it unhealthy, and the change is audited with source
// control_plane, once per change.
func TestServiceHealthSweep_ProbeCountsWardynsChecks(t *testing.T) {
	answer := "HTTP/1.1 503 Service Unavailable\r\n\r\n"
	var mu sync.Mutex
	f, pr, run := probeSweepFixture(t, stateFileJSON("healthy", "", 0, "null"), func() (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return answer, 0
	}, probedService("/healthz"))
	for i, want := range []struct {
		health   types.ServiceHealthState
		failures int
	}{{types.ServiceHealthStarting, 1}, {types.ServiceHealthStarting, 2}, {types.ServiceHealthUnhealthy, 3}} {
		f.sweep(t)
		p := f.st.status(run.ID).Probe
		if p == nil || p.Health != want.health || p.Failures != want.failures || p.Reason != "bad_status" || p.HTTPStatus != 503 {
			t.Fatalf("pass %d: probe %+v, want %s after %d failures", i+1, p, want.health, want.failures)
		}
		if h := f.st.status(run.ID).Health; h != types.ServiceHealthHealthy {
			t.Fatalf("pass %d: the sandbox's reading became %s", i+1, h)
		}
	}
	argv := pr.argvs()
	if len(argv) != 6 || argv[0][0] != "cat" || !slices.Equal(argv[1], []string{"socat", "-", "TCP:127.0.0.1:8080"}) {
		t.Fatalf("execs = %q, want the file then the port, each pass", argv)
	}
	rows := f.healthRows(t, "control_plane")
	if len(rows) != 1 || rows[0]["from"] != "starting" || rows[0]["to"] != "unhealthy" || rows[0]["reason"] != "bad_status" {
		t.Fatalf("control_plane rows = %v, want one starting to unhealthy", rows)
	}
	if n := len(f.healthRows(t, "sandbox")); n != 1 {
		t.Errorf("sandbox rows = %d, want the one starting to healthy", n)
	}

	mu.Lock()
	answer = "HTTP/1.1 200 OK\r\n\r\n"
	mu.Unlock()
	f.sweep(t)
	if p := f.st.status(run.ID).Probe; p.Health != types.ServiceHealthHealthy || p.Failures != 0 || p.Reason != "" || p.HTTPStatus != 200 {
		t.Fatalf("after a pass: %+v", *p)
	}
	if rows := f.healthRows(t, "control_plane"); len(rows) != 2 || rows[1]["to"] != "healthy" {
		t.Fatalf("control_plane rows = %v, want the change back to healthy", rows)
	}
	// Unchanged and not yet due: nothing is written.
	writes := f.st.writeCount(run.ID)
	f.sweep(t)
	if f.st.writeCount(run.ID) != writes {
		t.Errorf("an unchanged pass wrote the status")
	}
}

// A pass whose budget is spent runs no check, and a check that ran past it is not counted: a cut-short pass is
// no finding about the service.
func TestObserveServiceProbe_ACutShortPassCountsNothing(t *testing.T) {
	f, pr, run := probeSweepFixture(t, "", func() (string, int) { return "", 1 }, probedService(""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := f.srv.observeServiceProbe(ctx, run); got != nil {
		t.Fatalf("a done pass checked: %+v", *got)
	}
	if n := len(pr.argvs()); n != 0 {
		t.Fatalf("a done pass started %d execs", n)
	}
	if got := f.srv.observeServiceProbe(context.Background(), run); got == nil || got.finding != serviceCheckRefused {
		t.Fatalf("a live pass = %+v, want refused", got)
	}
}

// TestRenewableServiceHealth: with the check off the rule is exactly renewalHealth's; with it on, renewal needs
// both readings starting or healthy. A missing check reading is starting inside the supervisor's grace and
// unknown after it; a stale one is unknown; neither renews.
func TestRenewableServiceHealth(t *testing.T) {
	now := time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
	reading := func(h types.ServiceHealthState, ago time.Duration) *types.RunServiceProbe {
		return &types.RunServiceProbe{Health: h, ObservedAt: now.Add(-ago)}
	}
	states := []types.ServiceHealthState{types.ServiceHealthStarting, types.ServiceHealthHealthy, types.ServiceHealthUnhealthy, types.ServiceHealthUnknown}
	renews := func(h types.ServiceHealthState) bool {
		return h == types.ServiceHealthStarting || h == types.ServiceHealthHealthy
	}
	// Every pair of fresh readings, with the check on and off.
	for _, sandbox := range states {
		for _, probe := range states {
			run := leaseServiceRun(now, time.Minute, types.RunLimits{MaxEndAheadSec: 7200})
			run.Service = probedService("/healthz")
			run.ServiceStatus = &types.RunServiceStatus{Health: sandbox, ObservedAt: now, Probe: reading(probe, time.Minute)}
			if got, want := renewableServiceHealth(run, now), renews(sandbox) && renews(probe); got != want {
				t.Errorf("on: sandbox %s, probe %s: %v, want %v", sandbox, probe, got, want)
			}
			run.Service.Health.Probe = ""
			if got, want := renewableServiceHealth(run, now), renews(renewalHealth(run, now)); got != want {
				t.Errorf("off: sandbox %s, probe %s: %v, want renewalHealth's %v", sandbox, probe, got, want)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		age     time.Duration // since the run was admitted
		probe   *types.RunServiceProbe
		renewed bool
	}{
		{"no check reading inside the grace is starting", time.Minute, nil, true},
		{"no check reading after the grace is unknown", serviceSupervisorGrace + time.Second, nil, false},
		{"a stale check reading is unknown", time.Hour, reading(types.ServiceHealthHealthy, serviceStatusStale+time.Second), false},
		{"a check reading at the stale mark still counts", time.Hour, reading(types.ServiceHealthHealthy, serviceStatusStale), true},
		{"a check that could not run is unknown", time.Hour, &types.RunServiceProbe{Health: types.ServiceHealthUnknown, Reason: "not_run", ObservedAt: now}, false},
		{"a failure under the threshold keeps healthy", time.Hour, &types.RunServiceProbe{Health: types.ServiceHealthHealthy, Reason: "timeout", Failures: 2, ObservedAt: now}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := leaseServiceRun(now, time.Minute, types.RunLimits{MaxEndAheadSec: 7200})
			admitted := now.Add(-tc.age)
			run.AdmittedAt, run.CreatedAt = &admitted, admitted.Add(-time.Hour)
			run.Service = probedService("")
			run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: now, Probe: tc.probe}
			if got := renewableServiceHealth(run, now); got != tc.renewed {
				t.Fatalf("renewable = %v, want %v", got, tc.renewed)
			}
			if _, ok := planServiceRenewal(run, 0, now); ok != tc.renewed {
				t.Errorf("planServiceRenewal = %v, want %v", ok, tc.renewed)
			}
			if got := serviceRenewsLater(run, now); got != tc.renewed {
				t.Errorf("serviceRenewsLater = %v, want %v", got, tc.renewed)
			}
		})
	}
}

// TestRunLease_ASandboxThatSaysHealthyIsNotRenewedWhileWardynsCheckFails is the case the check exists for: the
// sandbox's file says healthy, Wardyn's check of the port is unhealthy, and the lease is not renewed. It renews
// once the check passes again.
func TestRunLease_ASandboxThatSaysHealthyIsNotRenewedWhileWardynsCheckFails(t *testing.T) {
	f := newServiceLeaseFixture(t, 30*time.Minute)
	f.st.run.Service = probedService("/healthz")
	f.st.run.ServiceStatus = &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: f.now,
		Probe: &types.RunServiceProbe{Health: types.ServiceHealthUnhealthy, Reason: "bad_status", HTTPStatus: 503, Failures: 3, ObservedAt: f.now}}
	end := f.endsAt()
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(end) {
		t.Fatalf("ends_at moved to %v while Wardyn's check fails", got)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.end.set")); n != 0 {
		t.Fatalf("%d run.end.set rows, want none", n)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.ending_soon")); n != 1 {
		t.Errorf("%d run.ending_soon rows, want the normal warning", n)
	}
	f.st.run.ServiceStatus.Probe = &types.RunServiceProbe{Health: types.ServiceHealthHealthy, HTTPStatus: 200, ObservedAt: f.now}
	f.sweep(t)
	if got := f.endsAt(); !got.Equal(f.now.Add(2 * time.Hour)) {
		t.Fatalf("ends_at = %v, want a renewal once both pass", got)
	}
	if h := leaseAuditData(t, f.audit.eventsFor(f.run.ID, "run.end.set")[0])["health"]; h != "healthy" {
		t.Errorf("health = %v, want the sandbox's healthy", h)
	}
}

// Wardyn's check is served as the sandbox's reading is: unknown/paused for a paused run, unknown/unreachable once
// its own observed_at is stale, observed_at, failures and status kept, the stored row untouched. A status with no
// check reading is served exactly as before.
func TestServiceProbeOnTheWire(t *testing.T) {
	now := time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
	paused := now.Add(-time.Minute)
	for _, tc := range []struct {
		name          string
		sandboxAt     time.Time
		probeAt       time.Time
		paused        *time.Time
		sandbox       types.ServiceHealthState
		probe, reason string
	}{
		{name: "fresh", sandboxAt: now, probeAt: now, sandbox: types.ServiceHealthHealthy, probe: "unhealthy", reason: "bad_status"},
		{name: "the check alone stale", sandboxAt: now, probeAt: now.Add(-serviceStatusStale - time.Second), sandbox: types.ServiceHealthHealthy, probe: "unknown", reason: "unreachable"},
		{name: "paused", sandboxAt: now, probeAt: now, paused: &paused, sandbox: types.ServiceHealthUnknown, probe: "unknown", reason: "paused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: tc.sandboxAt,
				Probe: &types.RunServiceProbe{Health: types.ServiceHealthUnhealthy, Reason: "bad_status", HTTPStatus: 503, Failures: 4, ObservedAt: tc.probeAt}}
			runs := []types.AgentRun{{ServiceStatus: stored, PausedAt: tc.paused}}
			projectServiceHealth(runs, now)
			got := runs[0].ServiceStatus
			if got.Health != tc.sandbox || string(got.Probe.Health) != tc.probe || got.Probe.Reason != tc.reason ||
				!got.Probe.ObservedAt.Equal(tc.probeAt) || got.Probe.Failures != 4 || got.Probe.HTTPStatus != 503 {
				t.Errorf("served %+v probe %+v", *got, *got.Probe)
			}
			if stored.Health != types.ServiceHealthHealthy || stored.Probe.Health != types.ServiceHealthUnhealthy || stored.Probe.Reason != "bad_status" {
				t.Errorf("the stored status changed: %+v probe %+v", *stored, *stored.Probe)
			}
		})
	}
	plain := &types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: now}
	runs := []types.AgentRun{{ServiceStatus: plain}}
	projectServiceHealth(runs, now)
	if runs[0].ServiceStatus != plain || plain.Probe != nil {
		t.Errorf("a fresh status with no check reading was rewritten: %+v", *runs[0].ServiceStatus)
	}
}

// The workloads view reads the same pair: the check's reading is served beside the sandbox's, projected the same
// way, the state shows the worse of the two, and the custom resource's phase is unhealthy when the check is.
func TestWorkloadViewCarriesWardynsCheck(t *testing.T) {
	now := time.Date(2020, 10, 29, 12, 0, 0, 0, time.UTC)
	runID := uuid.New()
	v := store.WorkloadView{Workload: store.Workload{Name: "api", Owner: "maya", Generation: 1, CurrentGeneration: 1, CurrentRunID: &runID},
		RunState: types.RunRunning, ServiceHealth: "healthy", ServiceObservedAt: &now,
		ServiceProbe: &types.RunServiceProbe{Health: types.ServiceHealthUnhealthy, Reason: "refused", Failures: 3, ObservedAt: now}}
	st := viewServiceStatus(v, now)
	if st == nil || st.Health != types.ServiceHealthHealthy || st.Probe == nil || st.Probe.Health != types.ServiceHealthUnhealthy {
		t.Fatalf("view status = %+v", st)
	}
	if got := workloadOut(v, workspaceReadFull, now).State; got != "RUNNING/unhealthy" {
		t.Errorf("state = %q, want the worse of the two", got)
	}
	if got := workloadStatusOf(v, now).Phase; got != runnerwire.WorkloadPhaseUnhealthy {
		t.Errorf("phase = %q, want unhealthy", got)
	}
	v.ServiceProbe = nil
	if got := workloadOut(v, workspaceReadFull, now).State; got != "RUNNING/healthy" {
		t.Errorf("state with no check reading = %q, want the sandbox's", got)
	}
	// Opted in with no reading yet: Starting inside the supervisor's grace from admission, Health unknown after it,
	// as renewal reads it; the same view with the check off reads the sandbox's word alone.
	v.ServiceProbeOn = true
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{time.Minute, "RUNNING/starting"}, {serviceSupervisorGrace + time.Second, "RUNNING/unknown"}} {
		started := now.Add(-tc.age)
		v.RunStartedAt = &started
		if got := workloadOut(v, workspaceReadFull, now).State; got != tc.want {
			t.Errorf("opted in, no reading, started %s ago: state %q, want %q", tc.age, got, tc.want)
		}
	}
	if got := workloadStatusOf(v, now).Phase; got != runnerwire.WorkloadPhaseRunning {
		t.Errorf("phase with the check unknown = %q, want running (only unhealthy is Unhealthy)", got)
	}
	v.ServiceProbeOn = false
	if got := workloadOut(v, workspaceReadFull, now).State; got != "RUNNING/healthy" {
		t.Errorf("not opted in, started long ago: state %q, want the sandbox's", got)
	}
	v.ServiceProbeOn = true
	stale := now.Add(-serviceStatusStale - time.Minute)
	v.ServiceProbe = &types.RunServiceProbe{Health: types.ServiceHealthHealthy, ObservedAt: stale}
	if st := viewServiceStatus(v, now); st.Probe.Health != types.ServiceHealthUnknown || st.Probe.Reason != "unreachable" {
		t.Errorf("a stale check reading = %+v, want unknown/unreachable", *st.Probe)
	}
}
