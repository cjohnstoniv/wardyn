// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// passSeen reports whether a pass back reached the sandbox's sftp-server.
func passSeen(sb *folderSandboxRunner) *atomic.Bool {
	var seen atomic.Bool
	sb.onPass = func() { seen.Store(true) }
	return &seen
}

// A pass back moves the record to a generation of its own and, answered by the runner, marks it settled.
func TestPassBack_ACompletedPassIsSettledAtItsOwnGeneration(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Error("a completed pass kept the sandbox")
	}
	// The seed owns gen 1, the pass back gen 2.
	if st := cs.folderStatus(); st.Back == nil || st.Back.Error != "" || st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want the completed pass at gen 2, settled", st)
	}
}

// passFixture is computerFolderFixture seeded, with the record then set to gen and settled and a resume
// kept, and the folder's runner set by runnerAs.
type passFixture struct {
	s    *Server
	cs   *computerFolderStore
	sb   *folderSandboxRunner
	home string
}

func newPassFixture(t *testing.T, gen, settled int) *passFixture {
	t.Helper()
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.smu.Lock()
	cs.status.Gen, cs.status.Settled, cs.status.Resume = gen, settled, &types.FolderResume{Stalled: 1, Unsynced: 1}
	cs.smu.Unlock()
	agentWrites(t, sb, "a.txt")
	return &passFixture{s: s, cs: cs, sb: sb, home: filepath.Dir(filepath.Dir(app))}
}

// runnerAs connects the folder's runner advertising caps, or with caps nil takes it offline.
func (f *passFixture) runnerAs(t *testing.T, caps *runnerwire.Caps) {
	t.Helper()
	id := f.cs.runner.ID
	var ep runnerEndpoint
	if caps != nil {
		ep = folderRunnerEndpointCaps(t, id, *caps, f.home)
	}
	f.s.runnerStreams.subsMu.Lock()
	defer f.s.runnerStreams.subsMu.Unlock()
	if caps == nil {
		delete(f.s.runnerStreams.subs, id)
		return
	}
	f.s.runnerStreams.subs[id] = ep
}

var (
	olderRunnerCaps  = runnerwire.Caps{Folders: true, MachineName: "alice-laptop"}
	onePerRunnerCaps = runnerwire.Caps{Folders: true, FolderPassOnce: true, MachineName: "alice-laptop"}
)

// An older runner gets no pass back while its last one is unsettled: the record says so, stays unsettled,
// and the sandbox is kept for a resume. Once settled, the pass runs.
func TestPassBack_AnOlderRunnerPassesOnlyWhenTheLastPassIsSettled(t *testing.T) {
	f := newPassFixture(t, 2, 1)
	f.runnerAs(t, &olderRunnerCaps)
	seen := passSeen(f.sb)
	if !f.s.bringComputerFolderBack(context.Background(), f.cs.run.ID, "sbx") || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox kept", seen.Load())
	}
	if st := f.cs.folderStatus(); st.Gen != 3 || st.Settled != 1 || st.Resume == nil || st.Back == nil || !strings.Contains(st.Back.Error, "was not confirmed") {
		t.Errorf("record %+v, want gen 3 unsettled, the resume kept and the unconfirmed pass named", st)
	}

	f = newPassFixture(t, 2, 2)
	f.runnerAs(t, &olderRunnerCaps)
	seen = passSeen(f.sb)
	f.s.bringComputerFolderBack(context.Background(), f.cs.run.ID, "sbx")
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 3 || st.Settled != 3 {
		t.Errorf("pass %v, record %+v; want the pass at gen 3, settled", seen.Load(), st)
	}
}

// While the last pass is unsettled, the runner the pass is sent to decides it: one offline at the pass,
// then back as an older runner, gets no pass, and neither leaves the generation settled for a later one;
// a runner that says it runs one pass per run at a time gets the pass.
func TestPassBack_AnUnsettledPassGoesOnlyToARunnerThatRunsOnePerRun(t *testing.T) {
	f := newPassFixture(t, 2, 1)
	seen := passSeen(f.sb)
	back := func() bool { return f.s.bringComputerFolderBack(context.Background(), f.cs.run.ID, "sbx") }
	f.runnerAs(t, nil)
	if !back() || seen.Load() {
		t.Fatalf("offline: pass %v; want no pass and the sandbox kept", seen.Load())
	}
	if st := f.cs.folderStatus(); st.Gen != 3 || st.Settled != 1 || st.Resume == nil {
		t.Errorf("offline: record %+v, want gen 3 unsettled, the resume kept", st)
	}
	f.runnerAs(t, &olderRunnerCaps)
	if !back() || seen.Load() {
		t.Fatalf("older runner back: pass %v; want no pass and the sandbox kept", seen.Load())
	}
	if st := f.cs.folderStatus(); st.Gen != 4 || st.Settled != 1 || st.Resume == nil {
		t.Errorf("older runner back: record %+v, want gen 4 unsettled, the resume kept", st)
	}
	f.runnerAs(t, &onePerRunnerCaps)
	back()
	if st := f.cs.folderStatus(); !seen.Load() || st.Gen != 5 || st.Settled != 5 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("pass %v, record %+v; want the pass at gen 5, completed and settled", seen.Load(), st)
	}
}

// A pass the runner refused as busy is settled at its generation and kept for a resume.
func TestPassBack_ABusyRunnerIsTriedAgainLater(t *testing.T) {
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	// The seed owns gen 1; the pass back moves the record to gen 2.
	rec, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 1)
	if !ok || rec.Gen != 2 {
		t.Fatalf("bump = %+v %v", rec, ok)
	}
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen) {
		t.Fatal("a busy pass was not kept")
	}
	if st := cs.folderStatus(); st.Resume == nil || st.Settled != 2 || st.Back != nil {
		t.Errorf("record %+v, want a resume kept, settled at gen 2, no back", st)
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure"); ev == nil || !strings.Contains(string(ev.Data), "tries again") {
		t.Errorf("row %+v, want the busy pass audited", ev)
	}
	if !errors.Is(&runnerwire.Error{Code: runnerwire.CodeFolderPassBusy}, runner.ErrFolderPassBusy) {
		t.Error("a folder_pass_busy reply does not read as runner.ErrFolderPassBusy")
	}
}

// A bump from a generation the record is not at does not land: that pass back is another owner's.
func TestPassBack_ARecordThatMovedFirstRunsNoPass(t *testing.T) {
	s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	if _, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 5); ok {
		t.Error("a bump from a generation the record is not at landed")
	}
}

// A delayed owner whose generation another owner moved on writes nothing and leaves the sandbox up: owner
// A's pass is held while owner B moves the record on, passes and settles; A's record then does not land,
// and A does not let its caller stop B's sandbox.
func TestPassBack_ADelayedOwnerNeitherWritesNorStops(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	var nested atomic.Bool
	var bKept atomic.Bool
	sb.onPass = func() {
		if nested.Swap(true) {
			return
		}
		bKept.Store(s.bringComputerFolderBack(context.Background(), run.ID, "sbx"))
	}
	if !s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Error("the delayed owner let its caller stop the sandbox")
	}
	if bKept.Load() {
		t.Error("owner B's completed pass kept the sandbox")
	}
	// The seed owns gen 1, owner A gen 2, owner B gen 3.
	if st := cs.folderStatus(); st.Gen != 3 || st.Settled != 3 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("record %+v, want owner B's settled pass at gen 3 and nothing from A", st)
	}
}

// A delayed seed whose generation another owner moved on during its pass records, audits and publishes
// nothing: the run's first sync is left to that owner.
func TestSeed_ADelayedSeedNeitherRecordsNorPublishes(t *testing.T) {
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	var moved atomic.Bool
	sb.onPass = func() {
		if _, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 1); ok {
			moved.Store(true)
		}
	}
	s.seedComputerFolder(context.Background(), run, *cs.folder, 20*time.Second)
	if !moved.Load() {
		t.Fatal("the other owner did not move the record on during the seed's pass")
	}
	if st := cs.folderStatus(); st.Gen != 2 || st.Seed != nil {
		t.Errorf("record %+v, want gen 2 and no seed from the delayed one", st)
	}
	if _, seed, err := cs.RunLocalFolderSeed(context.Background(), run.ID); err != nil || seed.SeededAt != nil || seed.Error != "" {
		t.Errorf("first sync %+v %v, want none published", seed, err)
	}
	events := audit.snapshot()
	for _, outcome := range []string{"success", "failure"} {
		if findAudit(events, run.ID, "run.folder.transfer", outcome) != nil || findAudit(events, run.ID, "run.seed", outcome) != nil {
			t.Errorf("a %s pass or first sync was audited for the delayed seed", outcome)
		}
	}
}

// statusLandsAfterRead is computerFolderStore on which another owner's completed pass back lands once, just
// after the first read of the record.
type statusLandsAfterRead struct {
	*computerFolderStore
	once sync.Once
}

func (s *statusLandsAfterRead) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	rec, err := s.computerFolderStore.RunLocalFolderStatus(ctx, id)
	s.once.Do(func() {
		s.smu.Lock()
		defer s.smu.Unlock()
		s.status.Back, s.status.Settled = &types.FolderPassRecord{At: time.Now()}, s.status.Gen
	})
	return rec, err
}

// A pass back that completed between the read of the record and the bump is seen in the bumped record:
// no second pass runs, and the sandbox is left to its stop.
func TestPassBack_ABackThatLandedBeforeTheBumpRunsNoPass(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	seen := passSeen(sb)
	s.cfg.Store = &statusLandsAfterRead{computerFolderStore: cs}
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox left to its stop", seen.Load())
	}
	if st := cs.folderStatus(); st.Gen != 2 || st.Back == nil || st.Back.Error != "" {
		t.Errorf("record %+v, want the landed pass kept, moved to gen 2", st)
	}
}

// Only caps read from the runner that name one pass per run make it a new runner; unread caps are an
// older runner's.
func TestRunsOnePassPerRun_UnreadCapsAreAnOlderRunner(t *testing.T) {
	for _, tc := range []struct {
		caps runnerwire.Caps
		read bool
		want bool
	}{
		{runnerwire.Caps{FolderPassOnce: true}, true, true},
		{runnerwire.Caps{}, true, false},
		{runnerwire.Caps{FolderPassOnce: true}, false, false},
		{runnerwire.Caps{}, false, false},
	} {
		if got := runsOnePassPerRun(tc.caps, tc.read); got != tc.want {
			t.Errorf("runsOnePassPerRun(%+v, read %v) = %v, want %v", tc.caps, tc.read, got, tc.want)
		}
	}
}
