// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedReleaseRunner is the start gate's runner whose ExecStream records each argv with the run's proxy start
// state at that moment, and exits with exit.
type seedReleaseRunner struct {
	*execOrderRunner
	smu      sync.Mutex
	streams  [][]string
	atStream []types.ProxyStartState
	exit     int
}

func (r *seedReleaseRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	state := r.d.st.get(r.d.run.ID, 1).State
	r.smu.Lock()
	r.streams = append(r.streams, spec.Argv)
	r.atStream = append(r.atStream, state)
	exit := r.exit
	r.smu.Unlock()
	return &runner.ExecSession{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: strings.NewReader(""),
		Stderr: strings.NewReader(""),
		Wait:   func() (int, error) { return exit, nil },
		Close:  func() error { return nil },
	}, nil
}

// releases is the proxy start state at each release exec, in order.
func (r *seedReleaseRunner) releases() []types.ProxyStartState {
	r.smu.Lock()
	defer r.smu.Unlock()
	var got []types.ProxyStartState
	for i, argv := range r.streams {
		if slices.Equal(argv, heldSeedReleaseArgv) {
			got = append(got, r.atStream[i])
		}
	}
	return got
}

func (d *dispatchReadback) dispatchInteractive() {
	d.srv.dispatchRun(context.Background(), d.run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: d.token, Image: "wardyn/claude-code:latest", Interactive: true,
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
}

// createdEnv is a copy of the env the sandbox was created with.
func (d *dispatchReadback) createdEnv() map[string]string {
	d.rn.fakeRunner.mu.Lock()
	defer d.rn.fakeRunner.mu.Unlock()
	return maps.Clone(d.rn.fakeRunner.lastSpec.Env)
}

// newHeldSeedGate is the start gate, hold on, whose release exec exits with exit.
func newHeldSeedGate(t *testing.T, exit int) (*dispatchReadback, *seedReleaseRunner) {
	t.Helper()
	d, rn := newStartGate(t, hashHex([]byte("bytes")), true)
	sr := &seedReleaseRunner{execOrderRunner: rn, exit: exit}
	d.srv.cfg.Runner = sr
	return d, sr
}

// On: an interactive container run's task is created held, with no release before the proxy's report and
// exactly one after it, at enforced.
func TestStartAfterReadback_HeldSeedStartsAfterTheReport(t *testing.T) {
	sha := hashHex([]byte("bytes"))
	d, sr := newHeldSeedGate(t, 0)
	done := make(chan struct{})
	go func() { d.dispatchInteractive(); close(done) }()
	d.answered(t)
	time.Sleep(30 * time.Millisecond) // several polls of the hold
	env := d.createdEnv()
	if env[envHeldSeed] == "" {
		t.Fatalf("created env has no %s: %v", envHeldSeed, env)
	}
	if _, ok := env[envInteractiveSeed]; ok {
		t.Fatalf("created env still has %s", envInteractiveSeed)
	}
	if n := len(sr.releases()); n != 0 {
		t.Fatalf("release execs before the report = %d, want 0", n)
	}
	if got := d.st.State(); got != types.RunStarting {
		t.Fatalf("run state before the report = %s, want STARTING", got)
	}
	postLoaded(t, d.srv, d.run.ID, d.jti, sha)
	waitDone(t, done)
	if got := sr.releases(); len(got) != 1 || got[0] != types.ProxyStartEnforced {
		t.Fatalf("start state at each release exec = %v, want one at enforced", got)
	}
	if got := d.st.State(); got == types.RunFailed || got == types.RunStarting {
		t.Errorf("run state = %s, want it started", got)
	}
	ev := findAudit(d.audit.snapshot(), d.run.ID, "run.exec", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"reason":"seed_release"`) {
		t.Fatalf("want a run.exec success row with reason seed_release; got %v", ev)
	}
}

// On: an image whose agent-run cannot hold the task (the release exits 3) fails the run with its sandbox
// stopped.
func TestStartAfterReadback_HeldSeedOnAnOldImageFails(t *testing.T) {
	sha := hashHex([]byte("bytes"))
	d, _ := newHeldSeedGate(t, 3)
	done := make(chan struct{})
	go func() { d.dispatchInteractive(); close(done) }()
	d.answered(t)
	postLoaded(t, d.srv, d.run.ID, d.jti, sha)
	waitDone(t, done)
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
	if d.rn.stopCount() < 1 {
		t.Errorf("sandbox stops = %d, want at least 1", d.rn.stopCount())
	}
	ev := findAudit(d.audit.snapshot(), d.run.ID, "run.exec", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), `"reason":"seed_release"`) || !strings.Contains(string(ev.Data), `"exit_code":3`) {
		t.Fatalf("want a run.exec failure row with reason seed_release and exit_code 3; got %v", ev)
	}
}

// On: an interactive VM run with a task is still refused before the create, its env untouched.
func TestStartAfterReadback_VMBootSeedStillRefused(t *testing.T) {
	d, _ := newHeldSeedGate(t, 0)
	run := d.run
	run.MachineKind = types.MachineLinuxVM
	spec := runner.SandboxSpec{Env: map[string]string{envInteractiveSeed: "echo hi"}}
	called := false
	if !d.srv.startAfterReadbackRefused(context.Background(), run, spec, func() { called = true }) {
		t.Fatal("a VM run with a boot seed was not refused")
	}
	if !called {
		t.Error("done was not called")
	}
	if want := map[string]string{envInteractiveSeed: "echo hi"}; !maps.Equal(spec.Env, want) {
		t.Errorf("env = %v, want %v", spec.Env, want)
	}
	d.wantCreateFailure(t, "boot_seed_before_readback")
}

// On: a container run's boot seed is moved to the held variable, and the run is not refused.
func TestStartAfterReadback_ContainerSeedMovedNotRefused(t *testing.T) {
	d, _ := newHeldSeedGate(t, 0)
	spec := runner.SandboxSpec{Env: map[string]string{envInteractiveSeed: "echo hi"}}
	called := false
	if d.srv.startAfterReadbackRefused(context.Background(), d.run, spec, func() { called = true }) {
		t.Fatal("a container run with a boot seed was refused")
	}
	if called {
		t.Error("done was called")
	}
	if want := map[string]string{envHeldSeed: "echo hi"}; !maps.Equal(spec.Env, want) {
		t.Errorf("env = %v, want %v", spec.Env, want)
	}
	if ev := findAudit(d.audit.snapshot(), d.run.ID, "run.create", "failure"); ev != nil {
		t.Errorf("unexpected run.create failure row: %v", ev)
	}
}

// Off: the boot seed is left where it is, and nothing is released.
func TestStartAfterReadback_OffLeavesTheSeed(t *testing.T) {
	d, sr := newHeldSeedGate(t, 0)
	d.srv.cfg.StartAfterReadback = false
	d.dispatchInteractive()
	env := d.createdEnv()
	if env[envInteractiveSeed] == "" {
		t.Fatalf("created env has no %s: %v", envInteractiveSeed, env)
	}
	if _, ok := env[envHeldSeed]; ok {
		t.Fatalf("created env has %s", envHeldSeed)
	}
	if n := len(sr.releases()); n != 0 {
		t.Fatalf("release execs = %d, want 0", n)
	}
}
