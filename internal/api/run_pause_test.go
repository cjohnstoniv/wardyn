// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pauseStore is dispatchTestStore plus store.RunPauser, with the PG
// conditions of MarkRunPaused.
type pauseStore struct {
	*dispatchTestStore
	open, waiting bool
	stamps        int
}

func (s *pauseStore) ListPauseCandidates(ctx context.Context) ([]store.PauseCandidate, time.Time, error) {
	run, _ := s.GetRun(ctx, s.run.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.State != types.RunRunning || run.LostAt != nil || run.SandboxRef == "" {
		return nil, time.Time{}, nil
	}
	return []store.PauseCandidate{{Run: run, OpenRequest: s.open, WaitingRequest: s.waiting}}, time.Time{}, nil
}

func (s *pauseStore) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.run.ActiveAt = &now
	s.stamps++
	return s.run.PausedAt != nil, nil
}

func (s *pauseStore) MarkRunPaused(_ context.Context, _ uuid.UUID, reason types.PauseReason, activeAt *time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.run.ActiveAt
	sameActive := (cur == nil && activeAt == nil) || (cur != nil && activeAt != nil && cur.Equal(*activeAt))
	if s.state != types.RunRunning || s.run.LostAt != nil || s.run.PausedAt != nil || !sameActive ||
		(reason == types.PauseWaiting && !s.waiting) || (reason == types.PauseIdle && s.open) {
		return false, nil
	}
	now := time.Now().UTC()
	s.run.PausedAt, s.run.PausedReason = &now, reason
	return true, nil
}

func (s *pauseStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.run.PausedAt != nil
	s.run.PausedAt, s.run.PausedReason = nil, ""
	return was, nil
}

func (s *pauseStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open, nil
}

func (s *pauseStore) paused() (*time.Time, types.PauseReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.PausedAt, s.run.PausedReason
}

// pauseRunner is fakeRunner that can freeze, with a per-class Freeze map and a
// canned cgroup CPU reading for the idle check.
type pauseRunner struct {
	*fakeRunner
	freeze         map[types.ConfinementClass]bool
	cpu            *float64 // SampleCPU's reading for every ref; nil: no reading
	cpuErr         error    // SampleCPU's error, with no readings
	batch          bool     // BatchSample: one call reads every ref
	gone           bool     // Status reads the sandbox as no longer running
	onFreeze       func()
	mu             sync.Mutex
	freezes, thaws int
	sampleCalls    int
	sampled        [][]string
}

// pct is a canned CPU reading, in percent of one core.
func pct(v float64) *float64 { return &v }

func (r *pauseRunner) BatchSample() bool { return r.batch }

func (r *pauseRunner) SampleCPU(_ context.Context, refs []string) (map[string]float64, error) {
	r.mu.Lock()
	r.sampleCalls++
	r.sampled = append(r.sampled, refs)
	r.mu.Unlock()
	if r.cpuErr != nil {
		return nil, r.cpuErr
	}
	out := map[string]float64{}
	if r.cpu != nil {
		for _, ref := range refs {
			out[ref] = *r.cpu
		}
	}
	return out, nil
}

// samples is how many SampleCPU calls the runner served and how many refs they asked for.
func (r *pauseRunner) samples() (calls, refs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sampled {
		refs += len(s)
	}
	return r.sampleCalls, refs
}

func (r *pauseRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	caps, err := r.fakeRunner.Capabilities(ctx)
	caps.Freeze = r.freeze
	return caps, err
}

func (r *pauseRunner) Status(ctx context.Context, ref string) (runner.Status, error) {
	if r.gone {
		return runner.Status{State: types.RunStopped, Message: "container not found"}, nil
	}
	return r.fakeRunner.Status(ctx, ref)
}

func (r *pauseRunner) FreezeSandbox(context.Context, string) error {
	r.mu.Lock()
	r.freezes++
	r.mu.Unlock()
	if r.onFreeze != nil {
		r.onFreeze()
	}
	return nil
}

func (r *pauseRunner) ThawSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.thaws++
	return nil
}

func (r *pauseRunner) counts() (freezes, thaws int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.freezes, r.thaws
}

const pauseOwner = "sub-pause-owner"

type pauseFixture struct {
	h     *harness
	srv   *Server
	st    *pauseStore
	rn    *pauseRunner
	audit *recRecorder
	run   types.AgentRun
}

// newPauseFixture is one RUNNING CC1 run with a sandbox, nothing stamped for
// quiet, on a runner whose CC1 freeze is verified.
func newPauseFixture(t *testing.T, quiet time.Duration, configure ...func(*Config)) *pauseFixture {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.SandboxRef = pauseOwner, "ref-pause"
	active := time.Now().UTC().Add(-quiet)
	run.ActiveAt = &active
	h := newHarness(t)
	st := &pauseStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunRunning}}
	rn := &pauseRunner{fakeRunner: &fakeRunner{}, freeze: map[types.ConfinementClass]bool{types.CC1: true}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = rn
	for _, c := range configure {
		c(&cfg)
	}
	return &pauseFixture{h: h, srv: New(cfg), st: st, rn: rn, audit: h.audit, run: run}
}

func (f *pauseFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepRunPauses(context.Background()); err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
}

func (f *pauseFixture) rows(action, outcome string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action && ev.Outcome == outcome {
			out = append(out, ev)
		}
	}
	return out
}

// TestRunPause_FreezesOnlyAVerifiedClass is the fail-closed rule: a waiting run
// nobody has touched for longer than pauseWaitingAfter is frozen and marked
// paused when its class's freeze is verified — and never when the runner
// reports that class false, or does not report it at all (runsc/Kata, whose
// pause nobody has proven, or a capability read that failed).
func TestRunPause_FreezesOnlyAVerifiedClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		freeze map[types.ConfinementClass]bool
		pause  bool
	}{
		{"verified", map[types.ConfinementClass]bool{types.CC1: true}, true},
		{"reported false", map[types.ConfinementClass]bool{types.CC1: false}, false},
		{"not reported", map[types.ConfinementClass]bool{types.CC2: true}, false},
		{"no map", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
			f.st.open, f.st.waiting = true, true
			f.rn.freeze = tc.freeze
			f.sweep(t)

			freezes, _ := f.rn.counts()
			pausedAt, reason := f.st.paused()
			if tc.pause {
				if freezes != 1 || pausedAt == nil || reason != types.PauseWaiting {
					t.Fatalf("freezes = %d, paused = %v %q; want the run frozen and marked waiting", freezes, pausedAt, reason)
				}
				if rows := f.rows("run.pause", "success"); len(rows) != 1 || leaseAuditData(t, rows[0])["reason"] != "waiting" {
					t.Errorf("run.pause rows = %+v, want one with reason waiting", rows)
				}
				return
			}
			if freezes != 0 || pausedAt != nil {
				t.Fatalf("freezes = %d, paused = %v; a class whose freeze is not verified must never be frozen", freezes, pausedAt)
			}
		})
	}
}

// TestRunPause_WaitsOutTheDelay: a run with an open request that something
// happened in recently is left running; so is one with no request at all and
// no idle limit, however long nothing has happened.
func TestRunPause_WaitsOutTheDelay(t *testing.T) {
	f := newPauseFixture(t, pauseWaitingAfter-time.Minute)
	f.st.open, f.st.waiting = true, true
	f.sweep(t)
	g := newPauseFixture(t, 30*24*time.Hour)
	g.sweep(t)
	for name, fx := range map[string]*pauseFixture{"recent activity": f, "no request, no idle limit": g} {
		if freezes, _ := fx.rn.counts(); freezes != 0 {
			t.Errorf("%s: freezes = %d, want 0", name, freezes)
		}
	}
}

// TestRunPause_ActivityBetweenFreezeAndMarkWins: a keystroke that lands after
// the sweep read the run but before it marked it paused moves the presence
// clock, so the mark's compare fails and the agent is thawed again, with no
// run.pause row.
func TestRunPause_ActivityBetweenFreezeAndMarkWins(t *testing.T) {
	f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
	f.st.open, f.st.waiting = true, true
	f.rn.onFreeze = func() {
		if err := f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, pauseOwner, "presence"); err != nil {
			t.Errorf("markPresent: %v", err)
		}
	}
	f.sweep(t)
	freezes, thaws := f.rn.counts()
	if pausedAt, _ := f.st.paused(); freezes != 1 || thaws != 1 || pausedAt != nil {
		t.Fatalf("freezes %d thaws %d paused %v; want the freeze undone and the run left running", freezes, thaws, pausedAt)
	}
	if rows := f.rows("run.pause", "success"); len(rows) != 0 {
		t.Errorf("run.pause rows = %+v, want none", rows)
	}
}

// TestRunPause_IdleNeedsAQuietCPU: past pause_idle_after_sec an idle run
// pauses only when its CPU reads quiet; a busy reading, or one that could not
// be taken, leaves it running. The delay is floored at pauseDelayFloor.
func TestRunPause_IdleNeedsAQuietCPU(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet time.Duration
		cpu   *float64
		pause bool
	}{
		{"quiet", time.Hour, pct(2), true},
		{"busy", time.Hour, pct(40), false},
		{"unreadable", time.Hour, nil, false},
		{"inside the floor", pauseDelayFloor - time.Minute, pct(0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPauseFixture(t, tc.quiet)
			f.st.run.RunLimits.PauseIdleAfterSec = 60
			f.rn.cpu = tc.cpu
			f.sweep(t)
			pausedAt, reason := f.st.paused()
			if got := pausedAt != nil; got != tc.pause {
				t.Fatalf("paused = %v (%q), want %v", got, reason, tc.pause)
			}
			if tc.pause && reason != types.PauseIdle {
				t.Errorf("reason = %q, want idle", reason)
			}
		})
	}
}

// TestRunPause_IdleNeverLandsOnAnOpenRequest: a run with an open request is
// never paused idle, however quiet, so its close always finds it running or
// paused waiting. The sweep skips it (a re-auth request still inside its hold
// is open but does not count toward a waiting pause either), and a request
// raised between the CPU read and the mark fails the mark and thaws the agent.
func TestRunPause_IdleNeverLandsOnAnOpenRequest(t *testing.T) {
	for _, tc := range []struct {
		name          string
		openAtListing bool
		freezes       int
	}{
		{"open when listed", true, 0},
		{"opened before the mark", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPauseFixture(t, time.Hour)
			f.st.run.RunLimits.PauseIdleAfterSec = 60
			f.rn.cpu = pct(0)
			f.st.open = tc.openAtListing
			f.rn.onFreeze = func() { f.st.open = true }
			f.sweep(t)
			freezes, thaws := f.rn.counts()
			if pausedAt, reason := f.st.paused(); pausedAt != nil || freezes != tc.freezes || thaws != tc.freezes {
				t.Fatalf("paused %v %q, freezes %d thaws %d; want running, freezes = thaws = %d",
					pausedAt, reason, freezes, thaws, tc.freezes)
			}
			if rows := f.rows("run.pause", "success"); len(rows) != 0 {
				t.Errorf("run.pause rows = %+v, want none", rows)
			}
		})
	}
}

// TestRunPause_BackstopResumesWhenTheRequestCloses: a run paused waiting whose
// requests have all closed without a writer telling it (the expiry sweeper) is
// thawed by the next pass; one still waiting stays paused.
func TestRunPause_BackstopResumesWhenTheRequestCloses(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC().Add(-time.Minute)
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseWaiting
	f.st.open = true
	f.sweep(t)
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d with the request still open, want 0", thaws)
	}
	f.st.open = false
	f.sweep(t)
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Fatalf("thaws = %d after the request closed, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Errorf("still paused at %v", pausedAt)
	}
	rows := f.rows("run.resume", "success")
	if len(rows) != 1 || leaseAuditData(t, rows[0])["reason"] != "request_closed" {
		t.Errorf("run.resume rows = %+v, want one with reason request_closed", rows)
	}
}

// TestRunPause_ApprovalClosedResumesAtOnce: a writer closing the last open
// request thaws a paused run straight away, waiting or idle (an idle run can
// hold a request raised by traffic in flight when it froze); a run with a
// request still open stays paused.
func TestRunPause_ApprovalClosedResumesAtOnce(t *testing.T) {
	for _, reason := range []types.PauseReason{types.PauseWaiting, types.PauseIdle} {
		for _, open := range []bool{false, true} {
			f := newPauseFixture(t, time.Hour)
			paused := time.Now().UTC()
			f.st.run.PausedAt, f.st.run.PausedReason = &paused, reason
			f.st.open = open
			f.srv.approvalClosed(context.Background(), f.run.ID)
			want := 1
			if open {
				want = 0
			}
			if _, thaws := f.rn.counts(); thaws != want {
				t.Errorf("%s, open request %v: thaws = %d, want %d", reason, open, thaws, want)
			}
		}
	}
}

// TestResumeRun_OwnerThawsAForeignMemberCannot: POST /runs/{id}/resume thaws
// the owner's paused run once and moves its presence clock, audited as the
// owner; a member who does not own the run gets the byte-identical 404 and
// thaws nothing.
func TestResumeRun_OwnerThawsAForeignMemberCannot(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	path := "/api/v1/runs/" + f.run.ID.String() + "/resume"

	stranger := ssoSession(t, "sub-stranger", "stranger@corp.example", oidc.RoleUser)
	if w := doSSO(t, f.srv, http.MethodPost, path, stranger, ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign member resume = %d, want 404", w.Code)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d after a refused resume, want 0", thaws)
	}

	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
	if w := doSSO(t, f.srv, http.MethodPost, path, owner, ""); w.Code != http.StatusOK {
		t.Fatalf("owner resume = %d %s, want 200", w.Code, w.Body.String())
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Errorf("still paused at %v", pausedAt)
	}
	if _, thaws := f.rn.counts(); thaws != 1 || f.st.stamps != 1 {
		t.Errorf("thaws %d stamps %d; want the agent thawed once and the presence clock moved", thaws, f.st.stamps)
	}
	rows := f.rows("run.resume", "success")
	if len(rows) != 1 || rows[0].Actor != pauseOwner || leaseAuditData(t, rows[0])["reason"] != "resume" {
		t.Errorf("run.resume rows = %+v, want one by the owner with reason resume", rows)
	}
}

// TestResumeRun_RefusesAKeptRun pins run_pause.go's runIsKept check: a run
// lost to a reboot or an outage keeps RunState RUNNING (the kept-run design),
// so resume must refuse it by its lost mark, not by state alone — dropping
// that check would resume a run whose agent may not even exist any more,
// resurrecting it outside the revive path's re-checks.
func TestResumeRun_RefusesAKeptRun(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	lostAt := time.Now().UTC()
	f.st.run.LostAt, f.st.run.LostReason = &lostAt, types.LostReboot
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
	path := "/api/v1/runs/" + f.run.ID.String() + "/resume"

	if w := doSSO(t, f.srv, http.MethodPost, path, owner, ""); w.Code != http.StatusConflict {
		t.Fatalf("resume of a kept run = %d %s, want 409", w.Code, w.Body.String())
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d after a refused kept-run resume, want 0 — must never touch a run the revive path owns", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Error("kept run's pause mark was cleared by a refused resume; only MarkRunLost/MarkRunRevived may clear it")
	}
}

// TestPausedRun_WidgetsDoNotThawIt: the run page's polled reads answer 409 on
// a paused run and never thaw it — an open tab is not a person.
func TestPausedRun_WidgetsDoNotThawIt(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
	for _, p := range []string{"/resources", "/files"} {
		w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+p, owner, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), runPausedReadMsg) {
			t.Errorf("GET %s = %d %s, want 409 %q", p, w.Code, w.Body.String(), runPausedReadMsg)
		}
	}
	if _, thaws := f.rn.counts(); thaws != 0 || f.st.stamps != 0 {
		t.Errorf("thaws %d stamps %d; a widget read must not count as presence", thaws, f.st.stamps)
	}
}

// TestInternalActivity_StampsTheTokensRunOnly: the proxy's stream-activity
// report moves the presence clock of the run its token names, debounced, and
// never thaws; the approvals poll decision does not count as activity.
func TestInternalActivity_StampsTheTokensRunOnly(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseWaiting
	tok := f.h.mintRunToken(t, f.run.ID)

	if w := do(t, f.srv, http.MethodPost, "/api/v1/internal/activity", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", w.Code)
	}
	for range 2 {
		if w := do(t, f.srv, http.MethodPost, "/api/v1/internal/activity", tok, ""); w.Code != http.StatusNoContent {
			t.Fatalf("activity = %d %s, want 204", w.Code, w.Body.String())
		}
	}
	if f.st.stamps != 1 {
		t.Errorf("stamps = %d, want 1 (debounced)", f.st.stamps)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Errorf("thaws = %d; agent activity must never thaw a paused run", thaws)
	}
	if agentActivityDecision(ruleSourceApprovalsPoll) || agentActivityDecision(ruleSourceCredentialReauthTimeout) ||
		!agentActivityDecision("policy:allowlist") {
		t.Error("agentActivityDecision: the approvals poll and the re-auth timeout are not activity; an allow is")
	}
}

// TestResumeRun_AVanishedSandboxStaysPaused: a thaw of a container that no
// longer exists is a no-op success, so resume re-reads the sandbox's status.
// A sandbox that is not running is a failed resume: the run stays marked
// paused, a failure row names why, and Resume answers 502.
func TestResumeRun_AVanishedSandboxStaysPaused(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	f.rn.gone = true
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)

	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/resume", owner, ""); w.Code != http.StatusBadGateway {
		t.Fatalf("resume of a vanished sandbox = %d %s, want 502", w.Code, w.Body.String())
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Error("the pause was cleared over a sandbox that is not running")
	}
	if rows := f.rows("run.resume", "success"); len(rows) != 0 {
		t.Errorf("run.resume success rows = %+v, want none", rows)
	}
	rows := f.rows("run.resume", "failure")
	if len(rows) != 1 || !strings.Contains(leaseAuditData(t, rows[0])["error"].(string), "not running") {
		t.Errorf("run.resume failure rows = %+v, want one naming the sandbox not running", rows)
	}
}

// probeRunner models one sandbox whose CPU is shared by every exec in it: the
// run page's disk walk (script arm "") and the idle sampler run against the same
// cgroup, so a sample taken while a walk is under way reads busy. walk and
// sample, when set, run as that exec or read starts, and may block it.
type probeRunner struct {
	*pauseRunner
	walking       atomic.Int32
	walk, sample  func()
	skip          func()
	sampleStarted chan struct{}
	arms          []string
	armsMu        sync.Mutex
}

func (r *probeRunner) SampleCPU(_ context.Context, refs []string) (map[string]float64, error) {
	if r.sampleStarted != nil {
		close(r.sampleStarted)
	}
	if r.sample != nil {
		r.sample()
	}
	reading := 1.0
	if r.walking.Load() > 0 {
		reading = 40
	}
	out := map[string]float64{}
	for _, ref := range refs {
		out[ref] = reading
	}
	return out, nil
}

func (r *probeRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	arm := spec.Argv[4]
	r.armsMu.Lock()
	r.arms = append(r.arms, arm)
	r.armsMu.Unlock()
	if arm == "skip" && r.skip != nil {
		r.skip()
	}
	if arm == "" {
		r.walking.Add(1)
		defer r.walking.Add(-1)
		if r.walk != nil {
			r.walk()
		}
	}
	return &runner.ExecSession{Stdout: strings.NewReader("proc_count=2\n")}, nil
}

func newProbeFixture(t *testing.T) (*pauseFixture, *probeRunner, *http.Cookie) {
	t.Helper()
	f := newPauseFixture(t, time.Hour)
	f.st.run.RunLimits.PauseIdleAfterSec = 60
	pr := &probeRunner{pauseRunner: f.rn}
	f.srv.cfg.Runner = pr
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
	return f, pr, owner
}

// TestRunPause_OpenRunPageDoesNotDefeatIdlePause: the run page polls the Sandbox
// widget, whose disk walk is CPU in the sandbox's cgroup, so a walk under way
// when the idle sampler reads reads as an agent that is not quiet. The sampler
// waits the walk out and reads the agent alone, and the run pauses.
func TestRunPause_OpenRunPageDoesNotDefeatIdlePause(t *testing.T) {
	f, pr, owner := newProbeFixture(t)
	walkStarted, releaseWalk := make(chan struct{}), make(chan struct{})
	pr.walk = func() { close(walkStarted); <-releaseWalk }

	pollDone := make(chan int, 1)
	go func() {
		w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/resources", owner, "")
		pollDone <- w.Code
	}()
	<-walkStarted

	sweepDone := make(chan error, 1)
	go func() { sweepDone <- f.srv.sweepRunPauses(context.Background()) }()
	time.Sleep(100 * time.Millisecond) // the sampler has reached the walk
	close(releaseWalk)

	if code := <-pollDone; code != http.StatusOK {
		t.Fatalf("poll = %d, want 200", code)
	}
	if err := <-sweepDone; err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Fatal("run not paused: the run page's own disk walk was read as the agent's CPU")
	}
}

// TestRunPause_NoWalkStartsInsideTheIdleSample: a poll arriving while the idle
// sampler holds the sandbox's CPU window does not start a walk; it answers with
// the widget's other readings.
func TestRunPause_NoWalkStartsInsideTheIdleSample(t *testing.T) {
	f, pr, owner := newProbeFixture(t)
	pr.sampleStarted = make(chan struct{})
	releaseSample := make(chan struct{})
	pr.sample = func() { <-releaseSample }

	sweepDone := make(chan error, 1)
	go func() { sweepDone <- f.srv.sweepRunPauses(context.Background()) }()
	<-pr.sampleStarted

	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/resources", owner, "")
	close(releaseSample)
	if err := <-sweepDone; err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("poll = %d %s, want 200", w.Code, w.Body.String())
	}
	pr.armsMu.Lock()
	defer pr.armsMu.Unlock()
	if got := strings.Join(pr.arms, ","); got != "skip" {
		t.Errorf("script arms = %q, want a poll that walked nothing", got)
	}
}

// TestRunPause_RefusedWalkStillDelaysTheSample: a poll that arrives while the
// sampler is waiting out another walk is refused its own walk, but it still runs
// the script's other readings, which are CPU too. The sampler waits for it.
func TestRunPause_RefusedWalkStillDelaysTheSample(t *testing.T) {
	f, pr, owner := newProbeFixture(t)
	walkStarted, releaseWalk := make(chan struct{}), make(chan struct{})
	pr.walk = func() { close(walkStarted); <-releaseWalk }
	skipStarted, releaseSkip := make(chan struct{}), make(chan struct{})
	pr.skip = func() { close(skipStarted); <-releaseSkip }
	pr.sampleStarted = make(chan struct{})
	path := "/api/v1/runs/" + f.run.ID.String() + "/resources"

	poll1 := make(chan int, 1)
	go func() { poll1 <- doSSO(t, f.srv, http.MethodGet, path, owner, "").Code }()
	<-walkStarted
	sweepDone := make(chan error, 1)
	go func() { sweepDone <- f.srv.sweepRunPauses(context.Background()) }()
	time.Sleep(100 * time.Millisecond) // the sampler is waiting out the walk
	poll2 := make(chan int, 1)
	go func() { poll2 <- doSSO(t, f.srv, http.MethodGet, path, owner, "").Code }()
	<-skipStarted // refused its walk, reading the rest of the script
	close(releaseWalk)

	select {
	case <-pr.sampleStarted:
		t.Fatal("the sample opened while a refused poll's readings were still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseSkip)
	<-pr.sampleStarted
	for _, c := range []chan int{poll1, poll2} {
		if code := <-c; code != http.StatusOK {
			t.Fatalf("poll = %d, want 200", code)
		}
	}
	if err := <-sweepDone; err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
}

// TestRunPause_AbandonedWalkIsWaitedOut: a poll whose request ended mid-walk (a
// closed tab) leaves `timeout 2 du` running in the sandbox, so the sampler holds
// off for the grace period instead of reading that walk as the agent's CPU.
func TestRunPause_AbandonedWalkIsWaitedOut(t *testing.T) {
	prev := abandonedWalkGrace
	abandonedWalkGrace = 300 * time.Millisecond
	t.Cleanup(func() { abandonedWalkGrace = prev })
	f, pr, owner := newProbeFixture(t)
	ctx, disconnect := context.WithCancel(context.Background())
	pr.walk = func() { disconnect() }

	doSSOCtx(t, f.srv, ctx, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/resources", owner, "")
	start := time.Now()
	if !f.srv.pause.beginSample(context.Background(), f.run.ID) {
		t.Fatal("beginSample gave up")
	}
	f.srv.pause.endSample(f.run.ID)
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Errorf("sample opened after %v, want it to wait out the abandoned walk's grace", waited)
	}
}
