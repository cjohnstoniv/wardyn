// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macPrepRunner is a runner whose ExecStream plays the guest launcher's `agent-run --prepare`: it records
// each argv, answers with the exit code and stderr it was given, and counts Exec, which an interactive
// run must never reach (the completion watcher would end the run when prep exits).
type macPrepRunner struct {
	*fakeRunner
	mu        sync.Mutex
	streams   [][]string
	exit      int
	stderr    string
	streamErr error
}

var _ runner.Runner = (*macPrepRunner)(nil)

func (r *macPrepRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streams = append(r.streams, spec.Argv)
	if r.streamErr != nil {
		return nil, r.streamErr
	}
	return &runner.ExecSession{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: strings.NewReader(""),
		Stderr: strings.NewReader(r.stderr),
		Wait:   func() (int, error) { return r.exit, nil },
		Close:  func() error { return nil },
	}, nil
}

func (r *macPrepRunner) argvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.streams)
}

// macPrepStore is the dispatch test store with the status_detail write, which records where macPrepDone
// reads it back.
type macPrepStore struct {
	*dispatchTestStore
	dmu     sync.Mutex
	detail  string
	history []string
	// setErr fails the write of failOn (every write when failOn is empty).
	setErr error
	failOn string
}

func (s *macPrepStore) SetRunStatusDetail(_ context.Context, _ uuid.UUID, detail string) error {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	if s.setErr != nil && (s.failOn == "" || s.failOn == detail) {
		return s.setErr
	}
	s.detail = detail
	s.history = append(s.history, detail)
	return nil
}

func (s *macPrepStore) Detail() string {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	return s.detail
}

type macPrepRig struct {
	srv   *Server
	run   types.AgentRun
	st    *macPrepStore
	rn    *macPrepRunner
	audit *syncAudit
}

func newMacPrepRig(t *testing.T, kind types.MachineKind) *macPrepRig {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Interactive: true, MachineKind: kind,
	}
	rig := &macPrepRig{
		run:   run,
		st:    &macPrepStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunRunning}},
		rn:    &macPrepRunner{fakeRunner: &fakeRunner{}},
		audit: &syncAudit{},
	}
	cfg := baseTestConfig(h, rig.st)
	cfg.Runner, cfg.Audit = rig.rn, rig.audit
	rig.srv = New(cfg)
	return rig
}

func (r *macPrepRig) start() {
	r.startThen(nil)
}

// startThen starts the run with onPrepared as startAgentsOrIdle's hook.
func (r *macPrepRig) startThen(onPrepared func()) {
	r.srv.startAgentsOrIdle(context.Background(), r.run, nil, "fake-"+r.run.ID.String(), "", true, runnerExecution{}, nil, onPrepared)
}

// A window asked for at start on an interactive macOS VM run is asked for once its prep has recorded done, and
// never when the prep failed: before then every open is refused as preparing.
func TestStartAgentsOrIdle_MacOSInteractiveOnPreparedAfterPrepDone(t *testing.T) {
	rig := newMacPrepRig(t, types.MachineMacOSVM)
	var at []string
	rig.startThen(func() { at = append(at, rig.st.Detail()) })
	if !slices.Equal(at, []string{statusDetailMacPrepDone}) {
		t.Fatalf("onPrepared ran at status_detail %q, want once at %q", at, statusDetailMacPrepDone)
	}

	failed := newMacPrepRig(t, types.MachineMacOSVM)
	failed.rn.exit, failed.rn.stderr = 1, "clone failed"
	called := false
	failed.startThen(func() { called = true })
	if called {
		t.Fatal("onPrepared ran after a failed prep")
	}
}

var wantPrepArgv = []string{"/usr/local/bin/agent-run", "--prepare"}

// An interactive macOS VM run is prepared over ExecStream before it is awaiting attach: Exec is never
// called, the run stays RUNNING, and prep is on record only after it succeeded.
func TestStartAgentsOrIdle_MacOSInteractiveRunsPrepareOverExecStream(t *testing.T) {
	rig := newMacPrepRig(t, types.MachineMacOSVM)
	rig.start()

	if got := rig.rn.argvs(); len(got) != 1 || !slices.Equal(got[0], wantPrepArgv) {
		t.Fatalf("ExecStream argvs = %v, want one %v", got, wantPrepArgv)
	}
	if n := rig.rn.execCount(); n != 0 {
		t.Fatalf("Exec calls = %d, want none: Exec arms the completion watcher", n)
	}
	if got := rig.st.State(); got != types.RunRunning {
		t.Fatalf("run state = %q, want RUNNING", got)
	}
	if !slices.Equal(rig.st.history, []string{statusDetailMacPrepRunning, statusDetailMacPrepDone}) {
		t.Fatalf("status_detail writes = %v, want running then done", rig.st.history)
	}
	done := rig.run
	done.StatusDetail = rig.st.Detail()
	if !macPrepDone(done) {
		t.Fatalf("status_detail = %q, want the prep-done marker", done.StatusDetail)
	}
	for _, action := range []string{"run.exec", "run.interactive.start"} {
		if !rig.audit.has(rig.run.ID, action, "success") {
			t.Errorf("no %s success row", action)
		}
	}
	if rig.audit.has(rig.run.ID, "run.exec", "failure") {
		t.Error("a successful prep audited a failure")
	}
}

// A prep that fails fails the start with the launcher's sentence, audited as a failed task start is: a
// run.exec failure row, the sandbox stopped, the run FAILED. Prep is not on record and the run is never
// awaiting attach.
func TestStartAgentsOrIdle_MacOSInteractivePrepFailureFailsTheStart(t *testing.T) {
	const sentence = "the starting folder /Users/agent/x is missing or isn't a plain folder, so the run was not started."
	rig := newMacPrepRig(t, types.MachineMacOSVM)
	rig.rn.exit, rig.rn.stderr = 1, "agent-run: cloning o/r into /Users/agent/work/r\nagent-run: "+sentence+"\n"
	rig.start()

	if got := rig.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %q, want FAILED", got)
	}
	if got := rig.st.FailureHint(); got != sentence {
		t.Fatalf("failure reason = %q, want the launcher's sentence %q", got, sentence)
	}
	rows := rig.audit.eventsFor(rig.run.ID, "run.exec")
	if len(rows) != 1 || rows[0].Outcome != "failure" ||
		!strings.Contains(string(rows[0].Data), `"reason":"interactive_prepare"`) || !strings.Contains(string(rows[0].Data), `"exit_code":1`) {
		t.Fatalf("run.exec rows = %+v, want the one failure row for the prep", rows)
	}
	if rig.audit.has(rig.run.ID, "run.interactive.start", "success") {
		t.Error("a failed prep went on to await attach")
	}
	if d := rig.st.Detail(); macPrepDone(types.AgentRun{StatusDetail: d}) {
		t.Errorf("status_detail = %q after a failed prep, want it not done", d)
	}
}

// A launcher that exits nonzero without a sentence, and an ExecStream that fails, both fail the start with
// a plain account, not an empty reason.
func TestStartAgentsOrIdle_MacOSInteractivePrepFailsWithoutALauncherSentence(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(*macPrepRunner)
		want string
	}{
		"exit without a sentence": {func(r *macPrepRunner) { r.exit = 127 }, "the machine's preparation failed (agent-run --prepare exited 127), so the run was not started."},
		"stream refused":          {func(r *macPrepRunner) { r.streamErr = errors.New("guest unreachable") }, "the machine could not be prepared: guest unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newMacPrepRig(t, types.MachineMacOSVM)
			tc.set(rig.rn)
			rig.start()
			if got := rig.st.State(); got != types.RunFailed {
				t.Fatalf("run state = %q, want FAILED", got)
			}
			if got := rig.st.FailureHint(); got != tc.want {
				t.Fatalf("failure reason = %q, want %q", got, tc.want)
			}
			if macPrepDone(types.AgentRun{StatusDetail: rig.st.Detail()}) || !rig.audit.has(rig.run.ID, "run.exec", "failure") {
				t.Errorf("detail %q, run.exec failure row %v", rig.st.Detail(), rig.audit.has(rig.run.ID, "run.exec", "failure"))
			}
		})
	}
}

// A prep stage that cannot be put on record fails the start: a done marker that never lands would keep the
// window closed for good, and a running marker that never lands leaves a restart nothing to find. A failed
// running write never reaches the guest.
func TestStartAgentsOrIdle_MacOSInteractivePrepNotRecordedFailsTheStart(t *testing.T) {
	for _, tc := range []struct {
		failOn  string
		streams int
	}{
		{statusDetailMacPrepRunning, 0},
		{statusDetailMacPrepDone, 1},
	} {
		t.Run(tc.failOn, func(t *testing.T) {
			rig := newMacPrepRig(t, types.MachineMacOSVM)
			rig.st.setErr, rig.st.failOn = errors.New("store down"), tc.failOn
			rig.start()

			if got := rig.st.State(); got != types.RunFailed {
				t.Fatalf("run state = %q, want FAILED", got)
			}
			if n := len(rig.rn.argvs()); n != tc.streams {
				t.Errorf("ExecStream calls = %d, want %d", n, tc.streams)
			}
			if rig.audit.has(rig.run.ID, "run.interactive.start", "success") || !rig.audit.has(rig.run.ID, "run.exec", "failure") {
				t.Error("an unrecorded prep was not audited as a failed start")
			}
			if macPrepDone(types.AgentRun{StatusDetail: rig.st.Detail()}) {
				t.Error("an unrecorded prep is marked done")
			}
		})
	}
}

// A container or Linux VM interactive run is not prepared by the control plane: no ExecStream, no Exec,
// no marker, and the start row says nothing of a prep.
func TestStartAgentsOrIdle_NonMacOSInteractiveIsUnchanged(t *testing.T) {
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineLinuxVM} {
		t.Run(string(kind), func(t *testing.T) {
			rig := newMacPrepRig(t, kind)
			rig.start()

			if got := rig.rn.argvs(); len(got) != 0 || rig.rn.execCount() != 0 {
				t.Fatalf("ExecStream argvs = %v, Exec calls = %d; want neither", got, rig.rn.execCount())
			}
			if got := rig.st.State(); got != types.RunRunning {
				t.Fatalf("run state = %q, want RUNNING", got)
			}
			if rig.st.Detail() != "" || len(rig.st.history) != 0 {
				t.Errorf("status_detail = %q (writes %v), want none", rig.st.Detail(), rig.st.history)
			}
			rows := rig.audit.eventsFor(rig.run.ID, "run.interactive.start")
			if len(rows) != 1 || rows[0].Outcome != "success" || strings.Contains(string(rows[0].Data), "prepared") {
				t.Fatalf("run.interactive.start rows = %+v, want the one plain success", rows)
			}
			if rig.audit.has(rig.run.ID, "run.exec", "success") {
				t.Error("a run.exec row for a run that execs nothing")
			}
		})
	}
}

// The launcher's sentence is read from the tail of its output, however much the launcher said before it.
func TestRunMacPrepareReadsTheTailOfTheOutput(t *testing.T) {
	rig := newMacPrepRig(t, types.MachineMacOSVM)
	rig.rn.exit = 1
	rig.rn.stderr = "agent-run: first\n" + strings.Repeat("noise noise noise\n", launcherSentenceMax/10) + "agent-run: the last one\n"
	rig.start()
	if got := rig.st.FailureHint(); got != "the last one" {
		t.Fatalf("failure reason = %q, want the last sentence", got)
	}
}

// The launcher's sentence is its last "agent-run: " line, without the prefix; none is none.
func TestLauncherSentence(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"git said no\n":                       "",
		"agent-run: a\nnoise\nagent-run: b\n": "b",
		"  agent-run: spaced  \n":             "spaced",
		"agent-run: \n":                       "",
	} {
		if got := launcherSentence(in); got != want {
			t.Errorf("launcherSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// A restart during an interactive macOS VM run's prep leaves "prep: running" on its row; the sweep that
// adopts the run ends it with the sentence, through the shared terminal tail (run.reconcile audit, revoke,
// sandbox teardown). A run whose prep finished, or any other run, is adopted as before.
func TestSweepRunWatchers_FailsAnInterruptedMacOSPrep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     types.MachineKind
		detail   string
		wantFail bool
	}{
		{"interrupted", types.MachineMacOSVM, statusDetailMacPrepRunning, true},
		{"done", types.MachineMacOSVM, statusDetailMacPrepDone, false},
		{"linux vm", types.MachineLinuxVM, statusDetailMacPrepRunning, false},
		{"container", types.MachineContainer, statusDetailMacPrepRunning, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			run := execRun(t, "")
			run.Interactive, run.MachineKind, run.StatusDetail = true, tc.kind, tc.detail
			fr := &execExitRunner{fakeRunner: &fakeRunner{}}
			fake := &bootReconcileStore{run: run}
			audit := &syncAudit{}
			cfg := baseTestConfig(h, fake)
			cfg.Runner, cfg.Broker, cfg.Audit = fr, h.broker, audit
			srv := testServer(t, cfg)

			if err := srv.sweepRunWatchers(context.Background()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			to, transitioned := fake.finalTransition()
			if tc.wantFail {
				if !transitioned || to != types.RunFailed {
					t.Fatalf("transitioned=%v to=%q, want FAILED", transitioned, to)
				}
				if got := fake.hintValue(); got != "This run's setup was interrupted when the server restarted. Start a new run." {
					t.Errorf("failure reason = %q", got)
				}
				if !audit.has(run.ID, "run.reconcile", "success") {
					t.Error("no run.reconcile row")
				}
				if !slices.Contains(fr.stopped, run.SandboxRef) {
					t.Errorf("sandbox not stopped: %v", fr.stopped)
				}
				return
			}
			if transitioned {
				t.Fatalf("a run that was not mid-prep was finalized to %q", to)
			}
		})
	}
}

// An at-start window is asked for exactly once when asked: after the prep on an interactive macOS VM run, at
// RUNNING on any other run, and never when not asked.
func TestMacWindowAtStartWhen(t *testing.T) {
	for _, tc := range []struct {
		kind                   types.MachineKind
		interactive, asked     bool
		wantNow, wantAfterPrep bool
	}{
		{types.MachineMacOSVM, true, true, false, true},
		{types.MachineMacOSVM, false, true, true, false},
		{types.MachineContainer, true, true, true, false},
		{types.MachineContainer, false, true, true, false},
		{types.MachineMacOSVM, true, false, false, false},
		{types.MachineMacOSVM, false, false, false, false},
		{types.MachineContainer, true, false, false, false},
	} {
		now, after := macWindowAtStartWhen(tc.kind, tc.interactive, tc.asked)
		if now != tc.wantNow || after != tc.wantAfterPrep {
			t.Errorf("kind %q interactive %v asked %v: now %v after prep %v, want %v %v",
				tc.kind, tc.interactive, tc.asked, now, after, tc.wantNow, tc.wantAfterPrep)
		}
	}
}
