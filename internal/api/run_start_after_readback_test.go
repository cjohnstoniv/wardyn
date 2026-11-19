// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// execOrderRunner records the run's proxy start state at each agent Exec.
type execOrderRunner struct {
	*readbackCreateRunner
	d      *dispatchReadback
	mu     sync.Mutex
	atExec []types.ProxyStartState
}

func (r *execOrderRunner) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	state := r.d.st.get(r.d.run.ID, 1).State
	r.mu.Lock()
	r.atExec = append(r.atExec, state)
	r.mu.Unlock()
	return r.readbackCreateRunner.Exec(ctx, ref, argv)
}

func (r *execOrderRunner) execs() []types.ProxyStartState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.ProxyStartState(nil), r.atExec...)
}

// newStartGate is a dispatch whose create answers sha at once, with the hold set to on.
func newStartGate(t *testing.T, sha string, on bool) (*dispatchReadback, *execOrderRunner) {
	t.Helper()
	d := newDispatchReadback(t, sha, false)
	d.srv.cfg.StartAfterReadback = on
	rn := &execOrderRunner{readbackCreateRunner: d.rn, d: d}
	d.srv.cfg.Runner = rn
	old := startReadbackPoll
	startReadbackPoll = 5 * time.Millisecond
	t.Cleanup(func() { startReadbackPoll = old })
	return d, rn
}

// answered waits until the driver's answer is recorded: the start is pending with its deadline.
func (d *dispatchReadback) answered(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if p := d.st.get(d.run.ID, 1); p.State == types.ProxyStartPending && p.Deadline != nil {
			return
		}
	}
	t.Fatal("the driver's answer was never recorded")
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return")
	}
}

// On: the run stays STARTING with no agent exec until its proxy's report lands; the agent then starts,
// with the start already enforced.
func TestStartAfterReadback_OnExecsOnlyAfterTheReport(t *testing.T) {
	sha := hashHex([]byte("bytes"))
	d, rn := newStartGate(t, sha, true)
	done := make(chan struct{})
	go func() { d.dispatch(); close(done) }()
	d.answered(t)
	time.Sleep(30 * time.Millisecond) // several polls of the hold
	if n := len(rn.execs()); n != 0 {
		t.Fatalf("agent execs before the report = %d, want 0", n)
	}
	if got := d.st.State(); got != types.RunStarting {
		t.Fatalf("run state before the report = %s, want STARTING", got)
	}
	postLoaded(t, d.srv, d.run.ID, d.jti, sha)
	waitDone(t, done)
	if got := rn.execs(); len(got) != 1 || got[0] != types.ProxyStartEnforced {
		t.Fatalf("start state at each agent exec = %v, want one exec at enforced", got)
	}
	if got := d.st.State(); got == types.RunFailed || got == types.RunStarting {
		t.Errorf("run state = %s, want it started", got)
	}
}

// Off: unchanged; the run goes RUNNING and the agent starts while the report is still pending.
func TestStartAfterReadback_OffIsUnchanged(t *testing.T) {
	d, rn := newStartGate(t, hashHex([]byte("bytes")), false)
	d.dispatch()
	if got := rn.execs(); len(got) != 1 || got[0] != types.ProxyStartPending {
		t.Fatalf("start state at each agent exec = %v, want one exec at pending", got)
	}
	if got := d.st.State(); got == types.RunFailed || got == types.RunStarting {
		t.Errorf("run state = %s, want it started", got)
	}
}

// On: a report of other bytes, or no report by the deadline, fails the run with its sandbox stopped
// and no agent exec.
func TestStartAfterReadback_FailureStartsNoAgent(t *testing.T) {
	sha := hashHex([]byte("bytes"))
	for name, fail := range map[string]func(d *dispatchReadback){
		"mismatch": func(d *dispatchReadback) { postLoaded(t, d.srv, d.run.ID, d.jti, hashHex([]byte("other bytes"))) },
		"deadline": func(d *dispatchReadback) {
			m := d.st.memProxyStarts
			m.pmu.Lock()
			m.clock = m.clock.Add(proxyReportGrace + time.Second)
			m.pmu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, rn := newStartGate(t, sha, true)
			done := make(chan struct{})
			go func() { d.dispatch(); close(done) }()
			d.answered(t)
			fail(d)
			waitDone(t, done)
			if n := len(rn.execs()); n != 0 {
				t.Fatalf("agent execs = %d, want 0", n)
			}
			if got := d.st.State(); got != types.RunFailed {
				t.Fatalf("run state = %s, want FAILED", got)
			}
			if d.rn.stopCount() < 1 {
				t.Error("the sandbox was not stopped")
			}
			if p := d.st.get(d.run.ID, 1); p.State != types.ProxyStartFailed {
				t.Errorf("start = %s, want failed", p.State)
			}
		})
	}
}

// On: a legacy start, which never reports, never starts the agent.
func TestStartAfterReadback_LegacyStartFails(t *testing.T) {
	d, rn := newStartGate(t, hashHex([]byte("bytes")), true)
	d.srv.cfg.ProxyImage = "ghcr.io/example/wardyn-proxy:0.10.0-rc.17"
	d.dispatch()
	if n := len(rn.execs()); n != 0 {
		t.Fatalf("agent execs = %d, want 0", n)
	}
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
	if d.rn.stopCount() != 1 {
		t.Errorf("sandbox stops = %d, want 1", d.rn.stopCount())
	}
}

// On: an interactive run with a task, whose sandbox would start the agent as it boots, is refused
// before any create.
func TestStartAfterReadback_BootSeedRefusedBeforeCreate(t *testing.T) {
	d, _ := newStartGate(t, hashHex([]byte("bytes")), true)
	d.srv.dispatchRun(context.Background(), d.run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: d.token, Image: "wardyn/claude-code:latest", Interactive: true,
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
	d.rn.fakeRunner.mu.Lock()
	creates := d.rn.fakeRunner.createCalls
	d.rn.fakeRunner.mu.Unlock()
	if creates != 0 {
		t.Fatalf("creates = %d, want 0", creates)
	}
	if got := d.st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want FAILED", got)
	}
}
