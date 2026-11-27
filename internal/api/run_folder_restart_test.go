// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fencedStartSandbox is folderSandboxRunner on a substrate that can start a stopped sandbox again with no network
// and its idle main process alone, as the server's Docker can. calls is the order of the calls that matter: "start",
// "pid1" for the read of PID 1, "stop" for the agent's stop, and "pass" for a pass's exec.
type fencedStartSandbox struct {
	*folderSandboxRunner
	pid1      string
	startErr  error
	restarted atomic.Bool
	mu        sync.Mutex
	calls     []string
}

func (r *fencedStartSandbox) note(c string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *fencedStartSandbox) callList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// StartFenced cuts the sandbox off its networks and starts it again, unless startErr.
func (r *fencedStartSandbox) StartFenced(context.Context, string) error {
	r.note("start")
	if r.startErr != nil {
		return r.startErr
	}
	r.proxyStopped.Store(true)
	r.ended = false
	r.restarted.Store(true)
	return nil
}

// AgentStatus is Docker's answer for the agent's exec once the sandbox was started again: the start dropped it, so it
// reads as neither running nor ended.
func (r *fencedStartSandbox) AgentStatus(ctx context.Context, ref, id string) (runner.Status, error) {
	if r.restarted.Load() {
		return runner.Status{}, errors.New("agent exec not found but container still running")
	}
	return r.folderSandboxRunner.AgentStatus(ctx, ref, id)
}

func (r *fencedStartSandbox) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if !r.ended {
		stop := func() (string, int) {
			r.note("stop")
			r.agentKilled.Store(true)
			return agentStoppedMark + "\n", 0
		}
		if sess, ok := agentExec(spec, r.pid1, stop); ok {
			if spec.Argv[0] == "cat" {
				r.note("pid1")
			}
			return sess, nil
		}
		if slices.Contains(spec.Argv, "-d") {
			r.note("pass")
		}
	}
	return r.folderSandboxRunner.ExecStream(ctx, ref, spec)
}

// leaseEnded is computerFolderFixture's run with the agent's files written and its sandbox stopped by a lease end,
// on a substrate that can start it again.
func leaseEnded(t *testing.T, files ...string) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *fencedStartSandbox, string) {
	t.Helper()
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, files...)
	sb.ended = true
	fs := &fencedStartSandbox{folderSandboxRunner: sb, pid1: taskIdlePID1}
	s.cfg.Runner = fs
	return s, cs, audit, run, fs, app
}

// A server sandbox a lease end stopped is started again with no network and its agent stopped, and its pass back
// brings the agent's work home; the sandbox is stopped after it (#3486). The sandbox has no egress and no agent
// running before the pass reads any of its files.
func TestFolderBack_ALeaseEndedSandboxIsStartedAgainForItsPass(t *testing.T) {
	s, cs, _, run, fs, app := leaseEnded(t, "lease.txt")
	var egress atomic.Value
	fs.onPass = func() { egress.Store(fmt.Sprint(fs.agentEgress())) }
	var firstRead sync.Once
	var read, cutAtRead, stoppedAtRead atomic.Bool
	fs.onRead = func(string) {
		firstRead.Do(func() {
			read.Store(true)
			cutAtRead.Store(errors.Is(fs.agentEgress(), errNoEgress))
			stoppedAtRead.Store(fs.agentKilled.Load() && slices.Contains(fs.callList(), "stop"))
		})
	}
	if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "lease.txt")); string(b) != "lease.txt by the agent" {
		t.Fatalf("lease.txt on the computer = %q; want the agent's (calls %v, back %+v)", b, fs.callList(), cs.folderStatus().Back)
	}
	if got := egress.Load(); got != errNoEgress.Error() {
		t.Errorf("agent egress as the pass started = %v; want %v", got, errNoEgress)
	}
	if !read.Load() || !cutAtRead.Load() || !stoppedAtRead.Load() {
		t.Errorf("at the pass's first file read: read %v, egress cut %v, agent stopped %v; want all true", read.Load(), cutAtRead.Load(), stoppedAtRead.Load())
	}
	want := []string{"start", "pid1", "stop", "pass"}
	if got := fs.callList(); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("calls = %v; want them to start %v", got, want)
	}
	if st := cs.folderStatus(); st.Back != nil && st.Back.Error != "" {
		t.Errorf("back record error = %q; want none", st.Back.Error)
	}
	if n := fs.stopCount(); n != 1 {
		t.Errorf("StopSandbox succeeded %d times, want 1: the sandbox started for the pass is stopped after it", n)
	}
}

// A sandbox that cannot be started again cleanly runs no pass, and its record says it had already stopped.
func TestFolderBack_ASandboxThatCannotStartCleanlyRunsNoPass(t *testing.T) {
	cases := map[string]struct {
		setup func(*fencedStartSandbox)
		calls []string
	}{
		"its start fails":                      {func(fs *fencedStartSandbox) { fs.startErr = errors.New("no such container") }, []string{"start"}},
		"its main process is not the idle one": {func(fs *fencedStartSandbox) { fs.pid1 = "claude\x00" }, []string{"start", "pid1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, fs, _ := leaseEnded(t, "lease.txt")
			tc.setup(fs)
			var passed atomic.Bool
			fs.onPass = func() { passed.Store(true) }
			if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
				t.Errorf("finalize: %v", err)
			}
			if passed.Load() {
				t.Error("a pass back ran in a sandbox that was not started again cleanly")
			}
			if got := fs.callList(); !slices.Equal(got, tc.calls) {
				t.Errorf("calls = %v; want %v", got, tc.calls)
			}
			assertFolderBackTornDown(t, cs, audit, run, fs.folderSandboxRunner, "no pass ran; the sandbox had already stopped, with its changes not brought back yet, uncounted")
		})
	}
}

// No start is asked for an exec-less sandbox, whose main process is its agent, nor for one that still runs.
func TestFolderBack_NoStartWhereItDoesNotApply(t *testing.T) {
	t.Run("its agent is the main process", func(t *testing.T) {
		s, cs, audit, run, fs, _ := leaseEnded(t, "lease.txt")
		cs.localFolderStore.run.AgentExecID = runner.MainProcessExecID
		if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
			t.Errorf("finalize: %v", err)
		}
		if got := fs.callList(); len(got) != 0 {
			t.Errorf("calls = %v; want none", got)
		}
		assertFolderBackTornDown(t, cs, audit, run, fs.folderSandboxRunner, "the sandbox had already stopped")
	})
	t.Run("it still runs", func(t *testing.T) {
		s, _, _, run, fs, app := leaseEnded(t, "lease.txt")
		fs.ended = false
		if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
			t.Errorf("finalize: %v", err)
		}
		if got := fs.callList(); slices.Contains(got, "start") {
			t.Errorf("calls = %v; want no start for a running sandbox", got)
		}
		if b, _ := os.ReadFile(filepath.Join(app, "lease.txt")); string(b) != "lease.txt by the agent" {
			t.Errorf("lease.txt on the computer = %q; want the agent's", b)
		}
	})
}

// A sandbox started again whose cut pass kept it runs its next pass: the agent's exec the start dropped reads as
// unknown, and the check that nothing but its init is left stands for it (#3486).
func TestFolderBack_ARestartedSandboxKeptForAResumeRunsItsNextPass(t *testing.T) {
	s, cs, _, run, fs, app := leaseEnded(t, "a.txt", "z.txt")
	fs.block = "z.txt"
	_, err := boundedFinalize(t, s, run, time.Second)
	assertFolderBackKept(t, err, cs, fs.folderSandboxRunner, types.FolderResume{Unsynced: 1})
	fs.block = ""
	if _, err := boundedFinalize(t, s, run, time.Second); err != nil {
		t.Errorf("finalize: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "z.txt")); string(b) != "z.txt by the agent" {
		back := cs.folderStatus().Back
		t.Errorf("z.txt on the computer = %q, want the agent's (back %+v)", b, back)
	}
	if back := cs.folderStatus().Back; back != nil && strings.Contains(back.Error, "status could not be read") {
		t.Errorf("back record = %q; want the resumed pass run", back.Error)
	}
}
