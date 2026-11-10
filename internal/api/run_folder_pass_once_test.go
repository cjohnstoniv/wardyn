// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
	if st := cs.folderStatus(); st.Back == nil || st.Back.Error != "" || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("record %+v, want the completed pass at gen 1, settled", st)
	}
}

// oldRunnerFixture is computerFolderFixture whose runner runs whatever pass it is sent (no
// Caps.FolderPassOnce), seeded, with the record at gen and settled.
func oldRunnerFixture(t *testing.T, gen, settled int) (*Server, *computerFolderStore, *folderSandboxRunner) {
	t.Helper()
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	home := filepath.Dir(filepath.Dir(app))
	s.runnerStreams.subs[cs.runner.ID] = folderRunnerEndpointCaps(t, cs.runner.ID, runnerwire.Caps{Folders: true, MachineName: "alice-laptop"}, home)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.smu.Lock()
	cs.status.Gen, cs.status.Settled, cs.status.Resume = gen, settled, &types.FolderResume{Stalled: 1, Unsynced: 1}
	cs.smu.Unlock()
	agentWrites(t, sb, "a.txt")
	return s, cs, sb
}

// An older runner gets no pass back while its last one is unsettled: the record says so and the sandbox is
// stopped, as a pass that cannot run is. Once settled, the pass runs.
func TestPassBack_AnOlderRunnerPassesOnlyWhenTheLastPassIsSettled(t *testing.T) {
	s, cs, sb := oldRunnerFixture(t, 2, 1)
	seen := passSeen(sb)
	if s.bringComputerFolderBack(context.Background(), cs.run.ID, "sbx") || seen.Load() {
		t.Fatalf("pass %v; want no pass and the sandbox left to its stop", seen.Load())
	}
	if st := cs.folderStatus(); st.Gen != 2 || st.Resume != nil || st.Back == nil || !strings.Contains(st.Back.Error, "was not confirmed") {
		t.Errorf("record %+v, want gen 2 and the unconfirmed pass named", st)
	}

	s, cs, sb = oldRunnerFixture(t, 2, 2)
	seen = passSeen(sb)
	s.bringComputerFolderBack(context.Background(), cs.run.ID, "sbx")
	if st := cs.folderStatus(); !seen.Load() || st.Gen != 3 || st.Settled != 3 {
		t.Errorf("pass %v, record %+v; want the pass at gen 3, settled", seen.Load(), st)
	}
}

// A pass the runner refused as busy is settled at its generation and kept for a resume.
func TestPassBack_ABusyRunnerIsTriedAgainLater(t *testing.T) {
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	gen, ok := s.bumpComputerFolderGen(context.Background(), run.ID, 0)
	if !ok || gen != 1 {
		t.Fatalf("bump = %d %v", gen, ok)
	}
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, gen) {
		t.Fatal("a busy pass was not kept")
	}
	if st := cs.folderStatus(); st.Resume == nil || st.Settled != 1 || st.Back != nil {
		t.Errorf("record %+v, want a resume kept, settled at gen 1, no back", st)
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
