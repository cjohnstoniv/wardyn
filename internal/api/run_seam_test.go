// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seamStopRunner is a runner that can stop a proxy alone and nothing else: any other Runner call panics
// on the nil embedded interface, except the two it counts because a keep test must see them not made.
type seamStopRunner struct {
	runner.Runner
	mu       sync.Mutex
	stopErr  error
	stops    int
	sandbox  int
	execs    int
	fenceErr error
	fences   int
}

func (r *seamStopRunner) StopProxy(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	return r.stopErr
}

func (r *seamStopRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandbox++
	return nil
}

func (r *seamStopRunner) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs++
	return nil, errors.New("seam: no exec")
}

// calls is every cut call the runner took.
func (r *seamStopRunner) calls() (stops, fences int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops, r.fences
}

// seamFenceRunner is seamStopRunner that also fences.
type seamFenceRunner struct{ *seamStopRunner }

func (r seamFenceRunner) FenceProxy(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fences++
	return r.fenceErr
}

// seamBareRunner stops no proxy and fences nothing.
type seamBareRunner struct{ runner.Runner }

type seamRecorder struct {
	mu   sync.Mutex
	rows []types.AuditEvent
}

func (r *seamRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, ev)
	return nil
}

func (r *seamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// seamServer is a server over rn, whose runner id is reported online while online is true.
func seamServer(t *testing.T, rn runner.Runner, online *atomic.Bool) (*Server, *seamRecorder) {
	t.Helper()
	rec := &seamRecorder{}
	srv := New(Config{Runner: rn, Audit: rec, RunnerOnline: func(uuid.UUID) bool { return online.Load() }})
	return srv, rec
}

// connect gives the server a runner session for id that delivered caps.
func seamConnect(t *testing.T, srv *Server, id uuid.UUID, caps runnerwire.Caps) {
	t.Helper()
	ep, _ := folderRunnerEndpointPeer(t, id, caps, t.TempDir())
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[id] = ep
}

func seamRun(state types.RunState, runnerID *uuid.UUID) types.AgentRun {
	return types.AgentRun{ID: uuid.New(), State: state, SandboxRef: "sbx", RunnerID: runnerID}
}

func TestCutAgent_RefusesALiveRun(t *testing.T) {
	rn := seamFenceRunner{&seamStopRunner{}}
	var on atomic.Bool
	srv, _ := seamServer(t, rn, &on)
	if _, err := srv.cutAgent(context.Background(), seamRun(types.RunRunning, nil)); err == nil {
		t.Fatal("a running run was cut")
	}
	if stops, fences := rn.calls(); stops+fences != 0 {
		t.Errorf("calls = %d stops, %d fences; want none", stops, fences)
	}
}

func TestCutAgent_VMIsUnsupportedWithNoCall(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, types.MachineMacOSVM} {
		rn := seamFenceRunner{&seamStopRunner{}}
		var on atomic.Bool
		on.Store(true)
		srv, _ := seamServer(t, rn, &on)
		id := uuid.New()
		seamConnect(t, srv, id, runnerwire.Caps{ProxyFence: true})
		for _, rid := range []*uuid.UUID{nil, &id} {
			run := seamRun(types.RunFailed, rid)
			run.MachineKind = kind
			if got, err := srv.cutAgent(context.Background(), run); got != cutUnsupported || err != nil {
				t.Errorf("%s runner=%v: cutAgent = %v, %v; want cutUnsupported, nil", kind, rid != nil, got, err)
			}
		}
		if stops, fences := rn.calls(); stops+fences != 0 {
			t.Errorf("%s: calls = %d stops, %d fences; want none", kind, stops, fences)
		}
	}
}

func TestCutAgent_StaleCapsOfAnOfflineRunnerAreNotConnected(t *testing.T) {
	rn := seamFenceRunner{&seamStopRunner{}}
	var on atomic.Bool
	on.Store(true)
	srv, _ := seamServer(t, rn, &on)
	id := uuid.New()
	seamConnect(t, srv, id, runnerwire.Caps{ProxyFence: true})
	if _, ok := srv.runnerConnected(id); !ok {
		t.Fatal("the runner is not connected while online")
	}
	on.Store(false)
	if _, ok := srv.runnerLastCaps(id); !ok {
		t.Fatal("the last caps are gone; the pin would not tell stale caps apart")
	}
	if _, ok := srv.runnerConnected(id); ok {
		t.Error("an offline runner reads as connected")
	}
	if _, err := srv.cutAgent(context.Background(), seamRun(types.RunFailed, &id)); !errors.Is(err, errRunnerNotHere) {
		t.Errorf("err = %v, want errRunnerNotHere", err)
	}
	if stops, fences := rn.calls(); stops+fences != 0 {
		t.Errorf("calls = %d stops, %d fences; want none", stops, fences)
	}
}

func TestCutAgent_RunnerWithoutCapStopsProxy(t *testing.T) {
	rn := seamFenceRunner{&seamStopRunner{}}
	var on atomic.Bool
	on.Store(true)
	srv, _ := seamServer(t, rn, &on)
	id := uuid.New()
	seamConnect(t, srv, id, runnerwire.Caps{})
	got, err := srv.cutAgent(context.Background(), seamRun(types.RunFailed, &id))
	if got != cutProxyStopped || err != nil {
		t.Errorf("cutAgent = %v, %v; want cutProxyStopped, nil", got, err)
	}
	if stops, fences := rn.calls(); stops != 1 || fences != 0 {
		t.Errorf("calls = %d stops, %d fences; want 1, 0", stops, fences)
	}
}

func TestCutAgent_OfflineAnswerIsNotObserved(t *testing.T) {
	rn := &seamStopRunner{stopErr: runner.ErrPendingOnRunner}
	var on atomic.Bool
	srv, _ := seamServer(t, rn, &on)
	_, err := srv.cutAgent(context.Background(), seamRun(types.RunFailed, nil))
	if !notObserved(err) {
		t.Errorf("err = %v, want one notObserved reads as not observed", err)
	}
	if notObserved(errors.New("the stop failed")) || notObserved(nil) {
		t.Error("an ordinary error or nil reads as not observed")
	}
}

func TestCutAgent_HostedFencesFirst(t *testing.T) {
	var on atomic.Bool
	rn := seamFenceRunner{&seamStopRunner{}}
	srv, _ := seamServer(t, rn, &on)
	if got, err := srv.cutAgent(context.Background(), seamRun(types.RunFailed, nil)); got != cutFenced || err != nil {
		t.Errorf("cutAgent = %v, %v; want cutFenced, nil", got, err)
	}
	if stops, fences := rn.calls(); stops != 0 || fences != 1 {
		t.Errorf("calls = %d stops, %d fences; want 0, 1", stops, fences)
	}
	rn = seamFenceRunner{&seamStopRunner{fenceErr: runner.ErrEndUnsupported}}
	srv, _ = seamServer(t, rn, &on)
	if got, err := srv.cutAgent(context.Background(), seamRun(types.RunFailed, nil)); got != cutProxyStopped || err != nil {
		t.Errorf("after ErrEndUnsupported: cutAgent = %v, %v; want cutProxyStopped, nil", got, err)
	}
	if stops, fences := rn.calls(); stops != 1 || fences != 1 {
		t.Errorf("after ErrEndUnsupported: calls = %d stops, %d fences; want 1, 1", stops, fences)
	}
}

func TestKeepable_Table(t *testing.T) {
	var on atomic.Bool
	on.Store(true)
	srv, _ := seamServer(t, &seamStopRunner{}, &on)
	fenced, bare, stale := uuid.New(), uuid.New(), uuid.New()
	seamConnect(t, srv, fenced, runnerwire.Caps{ProxyFence: true})
	seamConnect(t, srv, bare, runnerwire.Caps{})
	seamConnect(t, srv, stale, runnerwire.Caps{ProxyFence: true})
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id != stale }
	vm := seamRun(types.RunFailed, nil)
	vm.MachineKind = types.MachineMacOSVM
	for name, tc := range map[string]struct {
		run  types.AgentRun
		want bool
	}{
		"hosted container":         {seamRun(types.RunFailed, nil), true},
		"hosted VM":                {vm, false},
		"runner connected, cap":    {seamRun(types.RunFailed, &fenced), true},
		"runner with stale caps":   {seamRun(types.RunFailed, &stale), false},
		"runner connected, no cap": {seamRun(types.RunFailed, &bare), false},
	} {
		if got := srv.keepable(tc.run); got != tc.want {
			t.Errorf("%s: keepable = %v, want %v", name, got, tc.want)
		}
	}
	srv.cfg.Runner = seamBareRunner{}
	if srv.keepable(seamRun(types.RunFailed, nil)) {
		t.Error("a hosted runner that cannot stop a proxy alone is keepable")
	}
}

func TestCopyBackCut_Table(t *testing.T) {
	boom := errors.New("the stop failed")
	connected, bare, absent := uuid.New(), uuid.New(), uuid.New()
	vm := seamRun(types.RunFailed, nil)
	vm.MachineKind = types.MachineLinuxVM
	for name, tc := range map[string]struct {
		rn       func() (runner.Runner, *seamStopRunner)
		run      types.AgentRun
		want     error
		cuts     int // cut calls the runner takes
		rows     int
		noAgentX bool
	}{
		"hosted fence ok": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, nil), nil, 1, 0, false},
		"hosted stop proxy ok": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return r, r
		}, seamRun(types.RunFailed, nil), nil, 1, 0, false},
		"runner fenced": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, &connected), nil, 1, 0, false},
		"runner without the cap, stop proxy ok": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, &bare), nil, 1, 0, false},
		"runner stop proxy error": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{stopErr: boom}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, &bare), errProxyAloneUnsupported, 1, 1, false},
		"hosted stop proxy error": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{stopErr: boom}
			return r, r
		}, seamRun(types.RunFailed, nil), errProxyStopUnconfirmed, 1, 1, false},
		"runner not connected": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, &absent), errRunnerNotHere, 0, 0, false},
		"runner call answers offline": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{stopErr: runner.ErrRunnerOffline}
			return seamFenceRunner{r}, r
		}, seamRun(types.RunFailed, &bare), errRunnerNotHere, 1, 0, false},
		"virtual machine": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{}
			return seamFenceRunner{r}, r
		}, vm, errProxyAloneUnsupported, 0, 0, false},
		"Kubernetes server mode": {func() (runner.Runner, *seamStopRunner) {
			r := &seamStopRunner{stopErr: runner.ErrEndUnsupported}
			return r, r
		}, seamRun(types.RunFailed, nil), errProxyAloneUnsupported, 1, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			rn, probe := tc.rn()
			var on atomic.Bool
			on.Store(true)
			srv, rec := seamServer(t, rn, &on)
			seamConnect(t, srv, connected, runnerwire.Caps{ProxyFence: true})
			seamConnect(t, srv, bare, runnerwire.Caps{})
			err := srv.copyBackCut(context.Background(), tc.run, "run.reconcile")
			if !errors.Is(err, tc.want) {
				t.Errorf("copyBackCut = %v, want %v", err, tc.want)
			}
			stops, fences := probe.calls()
			if stops+fences != tc.cuts {
				t.Errorf("cut calls = %d stops, %d fences; want %d", stops, fences, tc.cuts)
			}
			if got := rec.count(); got != tc.rows {
				t.Errorf("failure rows = %d, want %d", got, tc.rows)
			}
			if tc.noAgentX {
				probe.mu.Lock()
				execs, sandboxes := probe.execs, probe.sandbox
				probe.mu.Unlock()
				if execs != 0 || sandboxes != 0 {
					t.Errorf("agent stop calls = %d execs, %d sandbox stops; want none", execs, sandboxes)
				}
			}
		})
	}
}

func TestLockLost_NoHoldIsNotLost(t *testing.T) {
	if lockLost(context.Background()) {
		t.Error("a ctx with no hold reads as lost")
	}
}

func TestLockLost_LocalHoldIsNotLost(t *testing.T) {
	hctx, release, err := db.NewLocalLocker().Lock(context.Background(), db.NewLockKey(db.RunOpLockClass, uuid.NewString()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if lockLost(hctx) {
		t.Error("a local hold reads as lost")
	}
}

func TestTeardownUnlessLost_DoesNotCallOnLostLock(t *testing.T) {
	was := lockLost
	defer func() { lockLost = was }()
	var calls int
	teardown := func(context.Context, string) error { calls++; return nil }
	lockLost = func(context.Context) bool { return true }
	if err := teardownUnlessLost(context.Background(), "sbx", teardown); !errors.Is(err, db.ErrLockLost) || calls != 0 {
		t.Errorf("lost: err = %v, calls = %d; want ErrLockLost, 0", err, calls)
	}
	lockLost = func(context.Context) bool { return false }
	if err := teardownUnlessLost(context.Background(), "sbx", teardown); err != nil || calls != 1 {
		t.Errorf("held: err = %v, calls = %d; want nil, 1", err, calls)
	}
}

func TestRunEndLoops_StartIsHarmless(t *testing.T) {
	var on atomic.Bool
	srv, rec := seamServer(t, &seamStopRunner{}, &on)
	srv.startRunEndLoops()
	if rec.count() != 0 {
		t.Error("the stub loops wrote an audit row")
	}
}
