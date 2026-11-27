// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// k8sServerRunner is lostRunner as the server's Kubernetes cluster answers through the orchestrator: StopProxy keeps
// nothing (runner.ErrEndUnsupported), and only the lost-run stop keeps the run's agent, answering lostErr.
type k8sServerRunner struct {
	*lostRunner
	lostErr   error
	lostStops []int
}

func (r *k8sServerRunner) StopProxy(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proxyStops++
	return runner.ErrEndUnsupported
}

func (r *k8sServerRunner) StopLostProxyAt(_ context.Context, _ string, gen int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lostStops = append(r.lostStops, gen)
	return r.lostErr
}

func (r *k8sServerRunner) lostStopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lostStops)
}

func newK8sServerLostFixture(t *testing.T) (*lostFixture, *k8sServerRunner) {
	t.Helper()
	f := newLostFixture(t)
	kr := &k8sServerRunner{lostRunner: f.lr}
	f.srv.cfg.Runner = kr
	return f, kr
}

// A server Kubernetes run lost in an outage is kept: the lost-run stop keeps its agent, the run stays RUNNING with
// run.lost kept:true, and the lease sweep re-asserts the stop without tearing it down. StopProxy is never asked.
func TestLostServerK8sRun_KeptThenReasserted(t *testing.T) {
	f, kr := newK8sServerLostFixture(t)
	f.ls.lapsed = true
	f.sweepTokens(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Fatalf("lost = %v %q, want the run marked lost (outage)", lostAt, reason)
	}
	if kr.lostStopCount() != 1 || kr.proxyStopCount() != 0 {
		t.Fatalf("StopLostProxyAt %d, StopProxy %d; want 1 and 0", kr.lostStopCount(), kr.proxyStopCount())
	}
	if f.rn.endCount() != 0 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Errorf("EndSandbox %d, StopSandbox %d, state %s; want 0, 0, RUNNING: the agent is kept",
			f.rn.endCount(), f.rn.stopCount(), f.st.State())
	}
	lost := f.audit.eventsFor(f.run.ID, "run.lost")
	if len(lost) != 1 || leaseAuditData(t, lost[0])["kept"] != true {
		t.Fatalf("run.lost events = %+v, want one with kept:true", lost)
	}

	f.sweepTokens(t)
	f.sweep(t)
	if kr.lostStopCount() != 2 || kr.proxyStopCount() != 0 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Errorf("later passes: StopLostProxyAt %d, StopProxy %d, StopSandbox %d, state %s; want the stop re-asserted once and the run kept",
			kr.lostStopCount(), kr.proxyStopCount(), f.rn.stopCount(), f.st.State())
	}
}

// A run the cluster cannot keep (runner.ErrEndUnsupported from the lost-run stop) is torn down as before.
func TestLostServerK8sRun_EndUnsupportedTearsDown(t *testing.T) {
	f, kr := newK8sServerLostFixture(t)
	kr.lostErr = runner.ErrEndUnsupported
	f.ls.lapsed = true
	f.sweepTokens(t)
	if kr.lostStopCount() != 1 {
		t.Fatalf("StopLostProxyAt = %d, want 1", kr.lostStopCount())
	}
	if f.st.State() != types.RunFailed || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d; want FAILED, 1: a run that cannot be kept is torn down", f.st.State(), f.rn.stopCount())
	}
}

// k8sServerStopRunner is seamStopRunner on the server's Kubernetes cluster: StopProxy keeps nothing, and the lost-run
// stop, which a terminal run's cut never asks, is counted.
type k8sServerStopRunner struct {
	*seamStopRunner
	lost atomic.Int32
}

func (r *k8sServerStopRunner) StopLostProxyAt(context.Context, string, int) error {
	r.lost.Add(1)
	return nil
}

// A terminal run's copy-back cut on the server's Kubernetes cluster still stops its sandbox: StopProxy keeps nothing,
// and the lost-run stop is never its path.
func TestCopyBackCut_K8sServerStillStops(t *testing.T) {
	r := &k8sServerStopRunner{seamStopRunner: &seamStopRunner{stopErr: runner.ErrEndUnsupported}}
	var on atomic.Bool
	srv, _ := seamServer(t, r, &on)
	if err := srv.copyBackCut(context.Background(), seamRun(types.RunFailed, nil), "run.reconcile"); !errors.Is(err, errProxyAloneUnsupported) {
		t.Errorf("copyBackCut = %v, want errProxyAloneUnsupported (the sandbox is stopped)", err)
	}
	if stops, _ := r.calls(); stops != 1 || r.lost.Load() != 0 {
		t.Errorf("StopProxy %d, StopLostProxyAt %d; want 1 and 0", stops, r.lost.Load())
	}
}

// k8sServerReviver answers as the server's Kubernetes cluster does, through the orchestrator, for a running run:
// every proxy replacement is refused, and it can keep a lost run's agent.
type k8sServerReviver struct{ *reviveRunner }

func (k8sServerReviver) CanReplaceProxy(context.Context, string) error {
	return runner.ErrReviveUnsupported
}

func (k8sServerReviver) ReplaceProxy(context.Context, string, []byte) error {
	return runner.ErrReviveUnsupported
}

func (k8sServerReviver) ReplaceProxyHashed(context.Context, string, []byte) (string, error) {
	return "", runner.ErrReviveUnsupported
}

func (k8sServerReviver) ReplaceProxyHashedAt(context.Context, string, []byte, int) (string, error) {
	return "", runner.ErrReviveUnsupported
}

func (k8sServerReviver) StopLostProxyAt(context.Context, string, int) error { return nil }

// A running server Kubernetes run is refused revive and restart with revive_unsupported, as before.
func TestReviveK8sServerRunningRunStillUnsupported(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.srv.cfg.Runner = k8sServerReviver{f.rr}

	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || body.Reason != "revive_unsupported" || body.Error != runner.ErrReviveUnsupported.Error() {
		t.Errorf("revive: %d %+v; want 409 revive_unsupported with runner.ErrReviveUnsupported's text", w.Code, body)
	}

	w = do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+f.run.ID.String()+`"]}`)
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(out.Results) != 1 {
		t.Fatalf("restart: %d %s; want 200 with one result", w.Code, w.Body.String())
	}
	if r := out.Results[0]; r.OK || r.LostAgain || r.Reason != "revive_unsupported" || r.Error != runner.ErrReviveUnsupported.Error() {
		t.Errorf("restart result %+v; want refused with revive_unsupported and not lost again", r)
	}
	if len(f.rr.replaced) != 0 {
		t.Error("a refused revive replaced the proxy")
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Error("a refused revive of a live run marked it lost")
	}
}
