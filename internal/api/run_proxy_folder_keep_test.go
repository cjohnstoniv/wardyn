// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readbackFolderStore is readbackStore with a computer folder's pass record, read as rec or failing with err.
type readbackFolderStore struct {
	*readbackStore
	rec types.RunLocalFolderStatus
	err error
}

func (s *readbackFolderStore) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	return s.rec, s.err
}

func (s *readbackFolderStore) SetRunLocalFolderStatus(context.Context, uuid.UUID, types.RunLocalFolderStatus) (bool, error) {
	return false, errors.New("this store records no pass")
}

// endedWithFailedStart is a run whose revived proxy failed its readback with that proxy's stop
// unresolved, then ended with its sandbox ref kept, its computer folder's record read as rec or
// failing with err. It answers the failed start's generation and the run's ref.
func endedWithFailedStart(t *testing.T, rec types.RunLocalFolderStatus, err error) (*readbackFixture, int, string) {
	t.Helper()
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	ref := f.st.run.SandboxRef
	f.st.mu.Unlock()
	if ref == "" {
		t.Fatal("the run has no sandbox ref")
	}
	f.srv.cfg.Store = &readbackFolderStore{readbackStore: f.rb, rec: rec, err: err}
	return f, gen, ref
}

// assertSandboxKept checks containment stopped the proxy stops more times and never the sandbox,
// and left the run's ref.
func assertSandboxKept(t *testing.T, f *readbackFixture, ref string, proxyStops, sandboxStops, more int) {
	t.Helper()
	if n := f.lr.proxyStopCount(); n != proxyStops+more {
		t.Errorf("StopProxy = %d, want %d", n, proxyStops+more)
	}
	if n := f.lr.stopCount(); n != sandboxStops {
		t.Errorf("StopSandbox = %d, want %d: the sandbox is kept for its computer folder's copy back", n, sandboxStops)
	}
	if run, _ := f.srv.cfg.Store.GetRun(context.Background(), f.run.ID); run.SandboxRef != ref {
		t.Errorf("ref = %q, want %q kept", run.SandboxRef, ref)
	}
}

var readbackKeptResume = types.RunLocalFolderStatus{Resume: &types.FolderResume{Unsynced: 2}}

// An ended run whose sandbox is kept for its computer folder's copy back has only its failed
// start's proxy stopped; the sandbox and its ref stay, and the start is contained once that stop
// is confirmed.
func TestReadbackContainment_AKeptFolderSandboxHasOnlyItsProxyStopped(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, readbackKeptResume, nil)
	f.setProxyStopErr(nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained", p.ContainedAt, p.ContainmentError)
	}
}

// A folder record that cannot be read counts as kept: the sandbox is not stopped on a guess.
func TestReadbackContainment_AnUnreadableFolderRecordKeepsTheSandbox(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, types.RunLocalFolderStatus{}, errors.New("the database is unreachable"))
	f.setProxyStopErr(nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
}

// An ended run with no folder record, or one with no resume, has its sandbox stopped as before.
func TestReadbackContainment_AnEndedRunNotKeptForItsFolderHasItsSandboxStopped(t *testing.T) {
	for name, tc := range map[string]struct {
		rec types.RunLocalFolderStatus
		err error
	}{
		"no record": {err: store.ErrNotFound},
		"no resume": {rec: types.RunLocalFolderStatus{Back: &types.FolderPassRecord{Error: "the pass failed"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, gen, _ := endedWithFailedStart(t, tc.rec, tc.err)
			f.setProxyStopErr(nil)
			proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
			if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
				t.Fatalf("containment = %+v, want contained", c)
			}
			if n := f.lr.stopCount(); n != sandboxStops+1 {
				t.Errorf("StopSandbox = %d, want %d", n, sandboxStops+1)
			}
			if n := f.lr.proxyStopCount(); n != proxyStops {
				t.Errorf("StopProxy = %d, want %d: the sandbox stop takes its proxy", n, proxyStops)
			}
		})
	}
}

// A kept sandbox's proxy stop that fails leaves the containment unresolved, recorded on the start
// and audited, with nothing claimed stopped and the sandbox not stopped in its place; the retry
// that stops it resolves it.
func TestReadbackContainment_AKeptFolderSandboxsFailedProxyStopStaysUnresolved(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, readbackKeptResume, nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	rows := len(f.audit.eventsFor(f.run.ID, "run.reconcile"))
	c := f.srv.containProxyStart(context.Background(), f.run.ID, gen)
	if c.Contained || !errors.Is(c.Err, errProxyStopUnconfirmed) {
		t.Fatalf("containment = %+v, want unresolved with errProxyStopUnconfirmed", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
	if p := f.active(t); p.ContainedAt != nil || !strings.Contains(p.ContainmentError, "proxy stop failed") {
		t.Errorf("start contained %v, error %q; want unresolved, naming the proxy stop", p.ContainedAt, p.ContainmentError)
	}
	evs := f.audit.eventsFor(f.run.ID, "run.reconcile")
	if len(evs) != rows+1 || evs[len(evs)-1].Outcome != "failure" || !strings.Contains(string(evs[len(evs)-1].Data), "daemon down") {
		t.Errorf("run.reconcile rows = %v, want one failure naming the stop's error", evs[rows:])
	}
	f.setProxyStopErr(nil)
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("the retry's containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 2)
}

// A running run that cannot be kept has its proxy stopped first and is then ended: a cut copy
// back of its computer folder keeps the sandbox, which is never stopped before that decision.
func TestReadbackContainment_ARunningRunsEndDecidesItsSandboxAfterTheProxyStop(t *testing.T) {
	s, cs, _, run, sb := hangingFolderBack(t)
	stoppedBefore := false
	sb.stopped = func() { stoppedBefore = stoppedBefore || cs.folderStatus().Back == nil }
	run.State, run.SandboxRef = types.RunRunning, "sbx"
	ctx, cancel := context.WithTimeout(context.Background(), runTeardownBudget+computerFolderAccountTime+300*time.Millisecond)
	defer cancel()
	if err := s.stopUnconfirmedProxy(ctx, run); err != nil {
		t.Fatalf("stopUnconfirmedProxy = %v, want the proxy stop confirmed", err)
	}
	if stoppedBefore {
		t.Error("the sandbox was stopped before the end's computer folder decision")
	}
	if !sb.proxyStopped.Load() {
		t.Error("the run's proxy was not stopped")
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept for its copy back", n)
	}
	if st := cs.folderStatus(); st.Resume == nil || *st.Resume != (types.FolderResume{Stalled: 1}) {
		t.Errorf("resume = %+v, want the cut pass's", st.Resume)
	}
	if got := cs.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
}

// failEndOnceStore fails the next write ending the run, as a database blip would.
type failEndOnceStore struct {
	*computerFolderStore
	fail atomic.Bool
}

func (s *failEndOnceStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to.IsTerminal() && s.fail.CompareAndSwap(true, false) {
		return false, errors.New("injected end-write failure")
	}
	return s.computerFolderStore.UpdateRunStateIf(ctx, id, from, to)
}

// A running run's end that could not be written leaves the containment unresolved, the run live and
// its sandbox standing; the retry ends it and makes the computer folder decision.
func TestReadbackContainment_AnUnwrittenEndStaysUnresolvedUntilARetryEndsTheRun(t *testing.T) {
	s, cs, _, run, sb := hangingFolderBack(t)
	fs := &failEndOnceStore{computerFolderStore: cs}
	fs.fail.Store(true)
	s.cfg.Store = fs
	run.State, run.SandboxRef = types.RunRunning, "sbx"
	if err := s.stopUnconfirmedProxy(context.Background(), run); !errors.Is(err, errRunEndUnwritten) {
		t.Fatalf("stopUnconfirmedProxy = %v, want errRunEndUnwritten", err)
	}
	if got, n := cs.State(), sb.stopCount(); got != types.RunRunning || n != 0 {
		t.Fatalf("state %s, StopSandbox %d; want the run still RUNNING with its sandbox standing", got, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTeardownBudget+computerFolderAccountTime+300*time.Millisecond)
	defer cancel()
	if err := s.stopUnconfirmedProxy(ctx, run); err != nil {
		t.Fatalf("the retry = %v, want the end recorded", err)
	}
	if got := cs.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept for its copy back", n)
	}
	if st := cs.folderStatus(); st.Resume == nil {
		t.Errorf("resume = %+v, want the cut pass's", st.Resume)
	}
}
