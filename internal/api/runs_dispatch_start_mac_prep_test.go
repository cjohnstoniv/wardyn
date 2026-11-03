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
	dmu    sync.Mutex
	detail string
	setErr error
}

func (s *macPrepStore) SetRunStatusDetail(_ context.Context, _ uuid.UUID, detail string) error {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.detail = detail
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
	r.srv.startAgentsOrIdle(context.Background(), r.run, nil, "fake-"+r.run.ID.String(), "", true, runnerExecution{}, nil)
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
	if d := rig.st.Detail(); d != "" {
		t.Errorf("status_detail = %q after a failed prep, want none", d)
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
			if rig.st.Detail() != "" || !rig.audit.has(rig.run.ID, "run.exec", "failure") {
				t.Errorf("detail %q, run.exec failure row %v", rig.st.Detail(), rig.audit.has(rig.run.ID, "run.exec", "failure"))
			}
		})
	}
}

// A prep that succeeded but could not be put on record fails the start: a window held for a marker that
// never lands would stay closed for good.
func TestStartAgentsOrIdle_MacOSInteractivePrepNotRecordedFailsTheStart(t *testing.T) {
	rig := newMacPrepRig(t, types.MachineMacOSVM)
	rig.st.setErr = errors.New("store down")
	rig.start()

	if got := rig.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %q, want FAILED", got)
	}
	if rig.audit.has(rig.run.ID, "run.interactive.start", "success") || !rig.audit.has(rig.run.ID, "run.exec", "failure") {
		t.Error("an unrecorded prep was not audited as a failed start")
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
			if rig.st.Detail() != "" {
				t.Errorf("status_detail = %q, want none", rig.st.Detail())
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
