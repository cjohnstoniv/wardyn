// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// folderStatusStore answers the run's computer folder record: "kept" (a pass back keeps its sandbox for a
// resume), "unreadable", or none; sets counts record writes.
type folderStatusStore struct {
	*failMarkStore
	mode string
	sets atomic.Int32
}

func (s *folderStatusStore) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	switch s.mode {
	case "kept":
		return types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1, Unsynced: 3}}, nil
	case "unreadable":
		return types.RunLocalFolderStatus{}, errors.New("injected folder record read failure")
	}
	return types.RunLocalFolderStatus{}, nil
}

func (s *folderStatusStore) SetRunLocalFolderStatus(context.Context, uuid.UUID, types.RunLocalFolderStatus) (bool, error) {
	s.sets.Add(1)
	return true, nil
}

// keptFolderFailedStart revives the fixture's run and has its new proxy report other bytes while another operation
// holds the run's lock, so the start fails uncontained. The run then stands as a finalize that kept its sandbox for a
// computer folder's pass back leaves it: terminal, its sandbox ref still recorded, its folder record as mode says.
// It answers the store and the start's generation.
func (f *readbackFixture) keptFolderFailedStart(t *testing.T, mode string) (*folderStatusStore, int) {
	t.Helper()
	fs := &folderStatusStore{failMarkStore: &failMarkStore{readbackStore: f.rb}, mode: mode}
	f.srv.cfg.Store = fs
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("take the run's lock")
	}
	code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes")))
	unlock()
	if code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	return fs, f.active(t).Generation
}

// A failed start of a run whose sandbox is kept for its computer folder's pass back has only its proxy stopped:
// the sandbox and the folder's record are left as they are, and the start is contained once that stop is confirmed.
func TestReadback_KeptFolderSandboxHasOnlyItsProxyStopped(t *testing.T) {
	f := newReadbackFixture(t)
	fs, gen := f.keptFolderFailedStart(t, "kept")
	sandboxStops, proxyStops := f.lr.stopCount(), f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained once the proxy stop is confirmed", c)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	if n := f.lr.proxyStopCount() - proxyStops; n != 1 {
		t.Errorf("proxy stops +%d, want 1", n)
	}
	if fs.sets.Load() != 0 {
		t.Error("the folder's record must be left as it is")
	}
}

// The overdue sweep contains the same start the same way: proxy only, and never a later sandbox stop.
func TestReadback_SweepKeepsAKeptFolderSandbox(t *testing.T) {
	f := newReadbackFixture(t)
	f.keptFolderFailedStart(t, "kept")
	sandboxStops := f.lr.stopCount()
	for range 2 {
		if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained by its proxy stop", p.ContainedAt, p.ContainmentError)
	}
	if overdue, err := f.rb.OverdueProxyStarts(context.Background()); err != nil || len(overdue) != 0 {
		t.Errorf("overdue starts = %+v (%v), want none", overdue, err)
	}
}

// A terminal run with no folder pass to resume still has its sandbox stopped: containment stays fail-closed.
func TestReadback_TerminalSandboxWithNoFolderResumeIsStopped(t *testing.T) {
	f := newReadbackFixture(t)
	f.keptFolderFailedStart(t, "")
	sandboxStops := f.lr.stopCount()
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 1 {
		t.Errorf("sandbox stops +%d, want 1", n)
	}
	if p := f.active(t); p.ContainedAt == nil {
		t.Error("the start must be contained once its sandbox stop lands")
	}
}

// A folder record that cannot be read keeps the sandbox, as every sweep does, but never settles the start: its proxy
// is stopped, and the start stays unresolved for a later pass to read the record again.
func TestReadback_UnreadableFolderRecordKeepsTheSandboxAndStaysUnresolved(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "unreadable")
	sandboxStops, proxyStops := f.lr.stopCount(), f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); c.Contained || c.Err == nil {
		t.Fatalf("containment = %+v, want unresolved on an unread folder record", c)
	}
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none while the folder record cannot be read", n)
	}
	if f.lr.proxyStopCount() <= proxyStops {
		t.Error("the proxy must still be stopped")
	}
	if p := f.active(t); p.ContainedAt != nil || p.ContainmentError == "" {
		t.Errorf("start contained %v, error %q; want unresolved with its error recorded", p.ContainedAt, p.ContainmentError)
	}
}

// orderRunner records the order of proxy and sandbox stops, each sandbox stop with the run's state as it was.
type orderRunner struct {
	*hashedReviveRunner
	mu    sync.Mutex
	calls []string
	state func() types.RunState
}

func (r *orderRunner) StopProxy(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.calls = append(r.calls, "proxy")
	r.mu.Unlock()
	return r.hashedReviveRunner.StopProxy(ctx, ref)
}

func (r *orderRunner) StopSandbox(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.calls = append(r.calls, "sandbox:"+string(r.state()))
	r.mu.Unlock()
	return r.hashedReviveRunner.StopSandbox(ctx, ref)
}

// A running hosted run that cannot be kept, whose start failed, has its proxy stopped before its end runs, so the
// end's computer folder decision (and its pass back) comes before any sandbox stop: every sandbox stop finds the
// run already ended, never one stopped right after its proxy.
func TestReadback_UnkeepableRunningRunStopsItsProxyBeforeItsEnd(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "")
	f.st.mu.Lock()
	f.st.state, f.st.run.Interactive, f.st.run.Placement = types.RunRunning, false, types.PlacementRemote
	f.st.mu.Unlock()
	ordered := &orderRunner{hashedReviveRunner: f.hr, state: f.st.State}
	f.srv.cfg.Runner = ordered
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained {
		t.Fatalf("containment = %+v, want contained once the end landed", c)
	}
	ordered.mu.Lock()
	calls := append([]string(nil), ordered.calls...)
	ordered.mu.Unlock()
	if len(calls) == 0 || calls[0] != "proxy" {
		t.Errorf("stops = %v, want the proxy stopped first", calls)
	}
	for _, c := range calls[1:] {
		if state, ok := strings.CutPrefix(c, "sandbox:"); ok && !isTerminalRunState(types.RunState(state)) {
			t.Errorf("stops = %v: the sandbox was stopped with the run %s, before its end", calls, state)
		}
	}
	if !isTerminalRunState(f.st.State()) {
		t.Errorf("state %s, want the run ended", f.st.State())
	}
}

// sweepProxyRunner is sweepAliveRunner that can also stop a proxy alone, failing with proxyErr.
type sweepProxyRunner struct {
	*sweepAliveRunner
	proxyErr error
	proxied  []string
}

func (r *sweepProxyRunner) StopProxy(_ context.Context, ref string) error {
	r.proxied = append(r.proxied, ref)
	return r.proxyErr
}

// keptSweepStore is sweepStore whose run kept has a computer folder's pass back that keeps its sandbox, and whose
// run unreadable has a folder record that cannot be read.
type keptSweepStore struct {
	*sweepStore
	kept, unreadable uuid.UUID
}

func (s *keptSweepStore) RunLocalFolderStatus(_ context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	switch id {
	case s.kept:
		return types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1}}, nil
	case s.unreadable:
		return types.RunLocalFolderStatus{}, errors.New("injected folder record read failure")
	}
	return types.RunLocalFolderStatus{}, nil
}

func (s *keptSweepStore) SetRunLocalFolderStatus(context.Context, uuid.UUID, types.RunLocalFolderStatus) (bool, error) {
	return true, nil
}

// newKeptSweep is a terminal-sandbox sweep over runs, every sandbox probed live, with the folder records fake answers.
func newKeptSweep(t *testing.T, fake *keptSweepStore) (*Server, *sweepProxyRunner, *harness) {
	t.Helper()
	h := newHarness(t)
	fake.written = h.audit
	rr := &sweepProxyRunner{sweepAliveRunner: &sweepAliveRunner{fakeRunner: &fakeRunner{}}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rr
	cfg.Broker = h.broker
	return New(cfg), rr, h
}

// runRows is the audit rows for runID.
func runRows(events []types.AuditEvent, runID uuid.UUID) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}

// The terminal-sandbox sweep leaves a sandbox kept for its computer folder's pass back with only its proxy stopped,
// and still stops one that is not kept.
func TestSweepTerminalSandboxes_LeavesAKeptFolderSandbox(t *testing.T) {
	kept, orphan := sweepRun(types.RunFailed, "sbx-kept"), sweepRun(types.RunFailed, "sbx-orphan")
	srv, rr, _ := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept, orphan}}, kept: kept.ID})
	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rr.stopped) != 1 || rr.stopped[0] != "sbx-orphan" || len(rr.proxied) != 1 || rr.proxied[0] != "sbx-kept" || swept != 1 {
		t.Errorf("StopSandbox %v, StopProxy %v, swept %d; want [sbx-orphan], [sbx-kept], 1", rr.stopped, rr.proxied, swept)
	}
}

// Two passes over a kept sandbox stop its proxy once each and leave the sandbox: a stop that lands writes no row,
// and the sandbox is never counted as swept, on either pass.
func TestSweepTerminalSandboxes_KeptFolderSandboxIsNotReapedEachPass(t *testing.T) {
	kept := sweepRun(types.RunFailed, "sbx-kept")
	srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept}}, kept: kept.ID})
	for pass := 1; pass <= 2; pass++ {
		swept, err := srv.SweepTerminalSandboxes(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if swept != 0 || len(rr.proxied) != pass || len(rr.stopped) != 0 {
			t.Errorf("pass %d: swept %d, StopProxy %v, StopSandbox %v; want 0, %d proxy stops, no sandbox stop",
				pass, swept, rr.proxied, rr.stopped, pass)
		}
		if rows := runRows(h.audit.snapshot(), kept.ID); len(rows) != 0 {
			t.Errorf("pass %d: rows %+v, want none for a proxy stop that landed", pass, rows)
		}
	}
}

// A folder record the sweep cannot read keeps the sandbox too, with its proxy stopped, and is not counted as swept.
func TestSweepTerminalSandboxes_UnreadableFolderRecordKeepsTheSandbox(t *testing.T) {
	run := sweepRun(types.RunFailed, "sbx-unread")
	srv, rr, _ := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{run}}, unreadable: run.ID})
	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 || len(rr.proxied) != 1 || len(rr.stopped) != 0 {
		t.Errorf("swept %d, StopProxy %v, StopSandbox %v; want 0, one proxy stop, no sandbox stop", swept, rr.proxied, rr.stopped)
	}
}

// A kept sandbox whose proxy stop fails is the sweep's sandbox.sweep failure row, never silent.
func TestSweepTerminalSandboxes_KeptFolderProxyStopFailureIsAudited(t *testing.T) {
	kept := sweepRun(types.RunFailed, "sbx-kept")
	srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept}}, kept: kept.ID})
	rr.proxyErr = errors.New("the daemon did not answer the proxy stop")
	if _, err := srv.SweepTerminalSandboxes(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := runRows(h.audit.snapshot(), kept.ID)
	if len(rows) != 1 || rows[0].Action != "sandbox.sweep" || rows[0].Outcome != "failure" ||
		!strings.Contains(string(rows[0].Data), "proxy_stop_error") || len(rr.stopped) != 0 {
		t.Errorf("rows %+v, StopSandbox %v; want one sandbox.sweep failure with proxy_stop_error and no sandbox stop", rows, rr.stopped)
	}
}

// A kept sandbox whose proxy stop fails stays unresolved, as the run.reconcile failure row naming that stop; the
// sandbox, and the files it is kept for, are not stopped.
func TestReadback_KeptFolderProxyStopFailureIsAuditedAndUnresolved(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "kept")
	sandboxStops := f.lr.stopCount()
	f.setProxyStopErr(errors.New("daemon down"))
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); c.Contained || c.Err == nil {
		t.Fatalf("containment = %+v, want unresolved while the proxy stop fails", c)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	var rows int
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.reconcile") {
		if ev.Outcome == "failure" && strings.Contains(string(ev.Data), `"proxy_stop_error"`) && strings.Contains(string(ev.Data), "daemon down") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d run.reconcile failure rows with proxy_stop_error, want 1", rows)
	}
}

// A terminal sandbox whose folder record cannot be read keeps its egress cut and its revoke on every substrate:
// it is kept only once its proxy is stopped alone, as a folder_record_error row; it is stopped where it cannot be
// kept (on a runner, a virtual machine), where its proxy cannot be stopped alone, and where that stop failed.
func TestSweepTerminalSandboxes_UnreadableFolderRecordFailsClosed(t *testing.T) {
	runnerID := uuid.New()
	for _, tc := range []struct {
		name     string
		noProxy  bool
		proxyErr error
		edit     func(*types.AgentRun)
		left     bool
		row      string
		proxied  int
	}{
		{name: "proxy stopped", left: true, row: "folder_record_error", proxied: 1},
		{name: "proxy stop failed", proxyErr: errors.New("the daemon did not answer the proxy stop"), row: "proxy_stop_error", proxied: 1},
		{name: "proxy stop unsupported", proxyErr: runner.ErrEndUnsupported, proxied: 1},
		{name: "no proxy stop", noProxy: true},
		{name: "on a runner", edit: func(r *types.AgentRun) { r.RunnerID = &runnerID }},
		{name: "virtual machine", edit: func(r *types.AgentRun) { r.MachineKind = types.MachineLinuxVM }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := sweepRun(types.RunFailed, "sbx-unread")
			if tc.edit != nil {
				tc.edit(&run)
			}
			srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{run}}, unreadable: run.ID})
			rr.proxyErr = tc.proxyErr
			if tc.noProxy {
				srv.cfg.Runner = rr.sweepAliveRunner
			}
			swept, err := srv.SweepTerminalSandboxes(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			wantSwept, wantStopped := 1, []string{"sbx-unread"}
			if tc.left {
				wantSwept, wantStopped = 0, nil
			}
			if swept != wantSwept || len(rr.proxied) != tc.proxied || !slices.Equal(rr.stopped, wantStopped) {
				t.Errorf("swept %d, StopProxy %v, StopSandbox %v; want %d, %d proxy stops, %v", swept, rr.proxied, rr.stopped, wantSwept, tc.proxied, wantStopped)
			}
			var failures []types.AuditEvent
			for _, ev := range runRows(h.audit.snapshot(), run.ID) {
				if ev.Outcome == "failure" {
					failures = append(failures, ev)
				}
			}
			if tc.row == "" && len(failures) != 0 || tc.row != "" && (len(failures) != 1 || failures[0].Action != "sandbox.sweep" || !strings.Contains(string(failures[0].Data), tc.row)) {
				t.Errorf("failure rows %+v, want %q", failures, tc.row)
			}
			if !slices.Contains(h.broker.revoked, run.ID) {
				t.Errorf("the run's credentials were not revoked: %v", h.broker.revoked)
			}
		})
	}
}
