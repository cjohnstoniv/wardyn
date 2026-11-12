// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pass back's record puts its outcome first and cuts only the reason: a 2000-rune runner reason cannot push
// how the sandbox ended (kept for a later sweep, or stopped with the count left behind) out of the record.
func TestFolderBackText_OutcomeSurvivesALongReason(t *testing.T) {
	long := strings.Repeat("é", 2000)
	bounded := strings.Repeat("é", maxFolderReason) + "..."

	_, kept := folderBackResume(nil, &runnerwire.FolderPass{}, long)
	const keptHead = "the sandbox is kept with its agent stopped and no egress, and a later sweep, about hourly, tries the sync again: "
	if kept != keptHead+bounded {
		t.Errorf("kept text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(kept)), maxFolderReason)
	}

	stopped := folderBackGivenUp(long, "2 passes in a row brought nothing back", 3)
	const stoppedHead = "the sandbox was stopped with changed files not brought back: 3, because 2 passes in a row brought nothing back: "
	if stopped != stoppedHead+bounded {
		t.Errorf("stopped text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(stopped)), maxFolderReason)
	}

	notPreserved := folderNotPreservedText(1, long, strings.Repeat("b", 2000))
	if n := len([]rune(notPreserved)); n > 120+2*(maxFolderReason+3)+4 || !strings.Contains(notPreserved, ": "+bounded) || !strings.HasSuffix(notPreserved, "; "+strings.Repeat("b", maxFolderReason)+"...") {
		t.Errorf("not-preserved text is %d runes, want each reason cut to %d runes", n, maxFolderReason)
	}

	for name, text := range map[string]string{"kept": kept, "stopped": stopped} {
		if got := folderPassRecord(time.Now(), runnerwire.FolderPass{}, text, true).Error; got != text {
			t.Errorf("%s: the record cut the text to %d runes; want all %d, outcome included", name, len([]rune(got)), len([]rune(text)))
		}
	}
}

// The count of what a stopped sandbox leaves behind comes first, before the cause.
func TestFolderBackText_GivenUpCountFirst(t *testing.T) {
	for _, tc := range []struct {
		unsynced int
		want     string
	}{
		{0, "the sandbox was stopped with its changes not brought back yet, uncounted, because why: reason"},
		{1, "the sandbox was stopped with changed files not brought back: 1, because why: reason"},
	} {
		if got := folderBackGivenUp("reason", "why", tc.unsynced); got != tc.want {
			t.Errorf("unsynced %d: got %q, want %q", tc.unsynced, got, tc.want)
		}
	}
}

// boundReason cuts at maxFolderReason runes and marks the cut: 0 runes is the empty-message line, 300 is kept
// whole, 301 and 2000 are cut to 300 and the marker.
func TestBoundReason_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "the sync reported a failure without a message"},
		{maxFolderReason, strings.Repeat("x", maxFolderReason)},
		{maxFolderReason + 1, strings.Repeat("x", maxFolderReason) + "..."},
		{2000, strings.Repeat("x", maxFolderReason) + "..."},
	} {
		if got := boundReason(strings.Repeat("x", tc.n)); got != tc.want {
			t.Errorf("%d runes: got %d runes %q, want %q", tc.n, len([]rune(got)), got, tc.want)
		}
	}
}

// bumpFailStore is computerFolderStore whose bump fails without landing (bumpErr), counting the record writes.
type bumpFailStore struct {
	*computerFolderStore
	bumpErr error
	sets    atomic.Int32
}

func (s *bumpFailStore) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, s.bumpErr
}

func (s *bumpFailStore) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	s.sets.Add(1)
	return s.computerFolderStore.SetRunLocalFolderStatusIf(ctx, id, guard, st)
}

// A pass back whose record cannot be moved to the pass's generation stops the sandbox, and says so: one failure
// row and one try at the record, at the generation it was read at. A silent stop leaves the run saying nothing.
func TestFolderBack_BumpStoreErrorRecordsAndAudits(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	bs := &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	f.s.cfg.Store = bs
	if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	const want = "no pass ran; the folder record could not be moved before its pass: its store could not write it; the sandbox was stopped with its changes not brought back yet, uncounted"
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
	if n := f.backRows(); n != 1 {
		t.Errorf("%d audit rows of passes back, want 1", n)
	}
	if n := bs.sets.Load(); n != 1 {
		t.Errorf("%d record writes, want 1", n)
	}
	if st := f.cs.folderStatus(); st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want it at gen 2, settled as before: the pass did not own a generation", st)
	}
}

// bumpComputerFolderGen's log tells a store error from a clean miss: only the miss "moved" before its pass.
func TestBumpLog_StoreErrorIsNotMoved(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	var logged lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	f.s.cfg.Store = &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	if _, ok, lost := f.s.bumpComputerFolderGen(context.Background(), f.run.ID, 2); ok || lost {
		t.Errorf("a store error answered ok %v lost %v, want neither", ok, lost)
	}
	out := logged.String()
	if !strings.Contains(out, "could not be moved before its pass") || !strings.Contains(out, "the bump failed") || strings.Contains(out, "pass record moved before its pass") {
		t.Errorf("a store error logged %q, want it as not moved, with the error", out)
	}

	f.s.cfg.Store = f.cs
	if _, ok, lost := f.s.bumpComputerFolderGen(context.Background(), f.run.ID, 5); ok || !lost {
		t.Errorf("a miss answered ok %v lost %v, want a clean miss", ok, lost)
	}
	if out := logged.String(); !strings.Contains(out[strings.LastIndex(out, "level=WARN"):], "pass record moved before its pass") {
		t.Errorf("a miss logged %q, want it as moved", out)
	}
}

// A store fault while recording a busy pass keeps the sandbox: the runner's busy reply proves another pass for
// the run is live. The fault is logged apart, naming the keep; a record another owner moved on is not a fault.
func TestBusy_StoreFaultKeepsAndLogs(t *testing.T) {
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	rec, ok, _ := s.bumpComputerFolderGen(context.Background(), run.ID, 1)
	if !ok {
		t.Fatalf("bump = %+v %v", rec, ok)
	}
	var logged lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	s.cfg.Store = &folderFaultStore{computerFolderStore: cs, setErr: errors.New("the write failed")}
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen) {
		t.Error("a store fault stopped the sandbox of a pass the runner still runs")
	}
	out := logged.String()
	if !strings.Contains(out, "recording a busy folder pass failed; the sandbox is kept because the runner is still running a pass for it") || !strings.Contains(out, "the write failed") {
		t.Errorf("the fault logged %q, want the keep and the error", out)
	}
	if st := cs.folderStatus(); st.Resume != nil {
		t.Errorf("record %+v, want the failed write to leave no resume", st)
	}
	if findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure") != nil {
		t.Error("a busy pass whose record did not land was audited")
	}

	// A record another owner moved on is a miss, not a fault: kept, and not logged as one.
	s.cfg.Store = cs
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen+5) {
		t.Error("a record moved by another owner stopped the sandbox")
	}
	if more := strings.TrimPrefix(logged.String(), out); strings.Contains(more, "recording a busy folder pass failed") {
		t.Errorf("a miss logged as a fault: %q", more)
	}
}

// scriptedFolderRunner is a folder runner whose pass has already ended when the server comes for its stream:
// sync_folder answers a stream id no call opened, and sync_folder_wait answers account. sent is the last
// sync_folder call's arguments.
type scriptedFolderRunner struct {
	account runnerwire.FolderPass
	mu      sync.Mutex
	sent    runnerwire.SyncFolderArgs
}

func (r *scriptedFolderRunner) args() runnerwire.SyncFolderArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// endpoint connects r as the folder runner id, sharing folders.
func (r *scriptedFolderRunner) endpoint(t *testing.T, id uuid.UUID) runnerEndpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New(id.String(), link, remote.Options{})
	orgPeer := link.Attach(ctx, org)
	peer := runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(_ context.Context, _ uint32, method string, raw json.RawMessage) (any, error) {
		switch method {
		case runnerwire.MethodSyncFolder:
			r.mu.Lock()
			defer r.mu.Unlock()
			if err := json.Unmarshal(raw, &r.sent); err != nil {
				return nil, err
			}
			return runnerwire.StreamResult{Stream: 4242}, nil
		case runnerwire.MethodSyncFolderWait:
			return r.account, nil
		}
		return nil, runnerwire.Refuse("scripted folder runner")
	}})
	go func() { _ = peer.Run(ctx) }()
	t.Cleanup(func() { orgPeer.Close(); peer.Close() })
	data, _ := json.Marshal(runnerwire.Caps{Folders: true, FolderPassOnce: true, MachineName: "alice-laptop"})
	if err := peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := sub.Caps(); ok {
			return runnerEndpoint{sub: sub, link: link}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's caps never arrived")
		}
	}
}

// scriptedFolderFixture is a computer folder run whose runner is a scriptedFolderRunner.
func scriptedFolderFixture(t *testing.T, account runnerwire.FolderPass) (*Server, *computerFolderStore, types.AgentRun, *scriptedFolderRunner) {
	t.Helper()
	s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	r := &scriptedFolderRunner{account: account}
	id := cs.runner.ID
	s.runnerStreams.subsMu.Lock()
	s.runnerStreams.subs[id] = r.endpoint(t, id)
	s.runnerStreams.subsMu.Unlock()
	run.SandboxRef = "sbx"
	return s, cs, run, r
}

// A pass that ended before the server took its stream is settled, with the runner's own account: its reason is
// the pass's error and its counts stand, so a copy back is recorded for what it did, not for a missing stream.
func TestComputerFolderPass_PassEndedIsSettled(t *testing.T) {
	account := runnerwire.FolderPass{Pulled: 1, Unsynced: 2, Error: "the pass ran out of time"}
	s, cs, run, _ := scriptedFolderFixture(t, account)
	got, cut, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, true, passGuard{})
	if err != nil || cut || !settled {
		t.Fatalf("computerFolderPass = cut %v settled %v err %v, want the pass settled, not cut, no error", cut, settled, err)
	}
	if got.Error != account.Error || got.Pulled != 1 || got.Unsynced != 2 {
		t.Errorf("the pass = %+v, want the runner's account %+v", got, account)
	}
}

// The server sends each pass its deadline: a pass back's less the window the runner's account needs to arrive
// in, a seed's as it is, and none without one. A pass back that read the seed's rule would leave the runner no
// time to answer.
func TestFolderPass_SendsItsDeadline(t *testing.T) {
	dl := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name     string
		pull     bool
		deadline bool
		want     int64
	}{
		{"back", true, true, dl.Add(-computerFolderAccountTime).UnixMilli()},
		{"seed", false, true, dl.UnixMilli()},
		{"back without one", true, false, 0},
		{"seed without one", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, run, r := scriptedFolderFixture(t, runnerwire.FolderPass{})
			ctx := context.Background()
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, dl)
				defer cancel()
			}
			if _, _, settled, err := s.computerFolderPass(ctx, ctx, run, *cs.folder, tc.pull, passGuard{}); err != nil || !settled {
				t.Fatalf("computerFolderPass: settled %v, err %v", settled, err)
			}
			if got := r.args(); got.DeadlineUnixMs != tc.want || got.Pull != tc.pull {
				t.Errorf("the runner was sent pull %v, deadline %d; want pull %v, deadline %d", got.Pull, got.DeadlineUnixMs, tc.pull, tc.want)
			}
		})
	}
	if computerFolderAccountTime != 5*time.Second {
		t.Errorf("computerFolderAccountTime = %v: this pin names 5 s", computerFolderAccountTime)
	}
}
