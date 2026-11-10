// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noLockAsked fails the test if any lock is asked for.
type noLockAsked struct{ t *testing.T }

func (l noLockAsked) Lock(ctx context.Context, k db.LockKey, _ time.Duration) (context.Context, func(), error) {
	l.t.Errorf("a lock was taken for %v", k)
	return ctx, func() {}, errors.New("no lock for this run")
}

func (l noLockAsked) TryLock(ctx context.Context, k db.LockKey) (context.Context, func(), bool, error) {
	l.t.Errorf("a lock was tried for %v", k)
	return ctx, func() {}, false, nil
}

// passSeen reports whether a pass back reached the sandbox's sftp-server.
func passSeen(sb *folderSandboxRunner) *atomic.Bool {
	var seen atomic.Bool
	sb.onPass = func() { seen.Store(true) }
	return &seen
}

// A run proved to owe no pass back (no folder, or one that is not both ways) ends as any run does: no
// lock is asked for, and its sandbox is stopped once.
func TestEnd_OrdinaryRunTakesNoLockAndStopsNormally(t *testing.T) {
	for name, dir := range map[string]string{"no folder": "", "a folder in only": types.ComputerFolderIn} {
		t.Run(name, func(t *testing.T) {
			s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderIn)
			if dir == "" {
				cs.folder = nil
			}
			s.locks.override = noLockAsked{t}
			if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
				t.Errorf("finalize: %v", err)
			}
			if n := sb.stopCount(); n != 1 {
				t.Errorf("StopSandbox succeeded %d times, want 1", n)
			}
		})
	}
}

// An end that does not hold the run's lock runs no pass back and writes no record: the sandbox is kept
// with its proxy stopped, and a holder's pass then brings the work home.
func TestEnd_AnUnheldEndKeepsAnOwedPassContained(t *testing.T) {
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	seen := passSeen(sb)
	err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
	if !errors.Is(err, errComputerFolderResumes) || sb.stopCount() != 0 || !sb.proxyStopped.Load() || seen.Load() {
		t.Fatalf("unheld finalize = %v, stops %d, proxy stopped %v, pass %v; want kept with its proxy stopped and no pass",
			err, sb.stopCount(), sb.proxyStopped.Load(), seen.Load())
	}
	if st := cs.folderStatus(); st.Back != nil || st.Gen != 0 {
		t.Fatalf("record %+v, want none written without the hold", st)
	}
	if bringBackHeld(t, s, run.ID, "sbx") {
		t.Error("the holder's completed pass kept the sandbox")
	}
	if b, _ := os.ReadFile(filepath.Join(app, "a.txt")); string(b) != "a.txt by the agent" {
		t.Errorf("a.txt on the computer = %q, want the agent's", b)
	}
	if st := cs.folderStatus(); st.Back == nil || st.Back.Error != "" || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("record %+v, want the completed pass at gen 1, settled", st)
	}
}

// An owner that loses its hold during the pass, while another owner moved the record on, neither
// writes its result nor stops the sandbox: the sandbox is kept, contained.
func TestPass_ALostOwnerNeitherWritesNorStops(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt")
	var lost atomic.Bool
	s.locks.held = func(ctx context.Context) error {
		if lost.Load() {
			return db.ErrLockLost
		}
		return db.LockHeld(ctx)
	}
	sb.onPass = func() {
		lost.Store(true)
		if _, ok, err := cs.BumpRunLocalFolderGen(context.Background(), run.ID, store.FolderStatusGuard{Gen: 1}); !ok || err != nil {
			t.Errorf("the other owner's bump: %v %v", ok, err)
		}
	}
	err := finalizeHeld(t, s, context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
	if !errors.Is(err, errComputerFolderResumes) || sb.stopCount() != 0 {
		t.Errorf("finalize = %v, stops %d; want kept, not stopped", err, sb.stopCount())
	}
	if st := cs.folderStatus(); st.Back != nil || st.Gen != 2 {
		t.Errorf("record %+v, want the other owner's gen 2 and no result from the lost one", st)
	}
}

// settleFirstStore lands a settle just before the owner's bump, as another owner finishing first does.
type settleFirstStore struct {
	*computerFolderStore
}

func (s *settleFirstStore) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard) (types.RunLocalFolderStatus, bool, error) {
	s.smu.Lock()
	s.status.Back, s.status.Resume = &types.FolderPassRecord{Files: 2, Refused: []types.FolderRefusal{}}, nil
	s.smu.Unlock()
	return s.computerFolderStore.BumpRunLocalFolderGen(ctx, id, guard)
}

// A settle that landed between the owner's read and its bump survives the bump, which moves only the
// generation on: the owner reads it done and runs no pass.
func TestPass_BumpKeepsASettleThatLandedFirst(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	s.cfg.Store = &settleFirstStore{cs}
	seen := passSeen(sb)
	if bringBackHeld(t, s, run.ID, "sbx") || seen.Load() {
		t.Errorf("pass %v; want the settled record skipped, the sandbox left to its stop", seen.Load())
	}
	if st := cs.folderStatus(); st.Back == nil || st.Back.Files != 2 || st.Gen != 1 {
		t.Errorf("record %+v, want the settle that landed first, at gen 1", st)
	}
}

// A pass this process is already running for the run is a no-attempt: no second pass, the record marks the
// generation settled, and the sandbox is kept.
func TestPass_ABusyPassIsANoAttemptAndKeeps(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	s.passLive.Store(run.ID, struct{}{})
	seen := passSeen(sb)
	if !bringBackHeld(t, s, run.ID, "sbx") || seen.Load() || sb.stopCount() != 0 {
		t.Errorf("pass %v, stops %d; want kept with no second pass", seen.Load(), sb.stopCount())
	}
	if st := cs.folderStatus(); st.Back != nil || st.Gen != 1 || st.Settled != 1 {
		t.Errorf("record %+v, want gen 1 settled with no result", st)
	}
}

// A runner that does not run one pass per run at a time gets no pass while its last one is unsettled; once
// settled, the next pass runs.
func TestPass_AnOlderRunnerPassesOnlyOnceSettled(t *testing.T) {
	s, cs, _, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	home := filepath.Dir(filepath.Dir(app))
	s.runnerStreams.subs[cs.runner.ID] = folderRunnerEndpointCaps(t, cs.runner.ID, runnerwire.Caps{Folders: true, MachineName: "alice-laptop"}, home)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.smu.Lock()
	cs.status.Gen, cs.status.Settled, cs.status.Resume = 2, 1, &types.FolderResume{Unsynced: 1}
	cs.smu.Unlock()
	seen := passSeen(sb)
	if !bringBackHeld(t, s, run.ID, "sbx") || seen.Load() || sb.stopCount() != 0 {
		t.Fatalf("pass %v, stops %d; want kept with no pass while gen 2 is unsettled", seen.Load(), sb.stopCount())
	}
	if st := cs.folderStatus(); st.Gen != 2 {
		t.Fatalf("record %+v, want gen 2 untouched", st)
	}
	cs.smu.Lock()
	cs.status.Settled = 2
	cs.smu.Unlock()
	bringBackHeld(t, s, run.ID, "sbx")
	if st := cs.folderStatus(); !seen.Load() || st.Gen != 3 {
		t.Errorf("pass %v, record %+v; want the pass at gen 3 once settled", seen.Load(), st)
	}
}

// On a sandbox the server cannot quiesce (one on a runner) the agent is still stopped before the pass.
func TestPass_QuiescesFirstOnEverySubstrate(t *testing.T) {
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	id := uuid.New()
	cs.run.RunnerID = &id
	agentWrites(t, sb, "a.txt")
	var running atomic.Bool
	sb.onPass = func() {
		agent, _ := sb.AgentStatus(context.Background(), "sbx", "agent-exec")
		running.Store(agent.State == types.RunRunning)
	}
	bringBackHeld(t, s, run.ID, "sbx")
	if running.Load() || !sb.agentKilled.Load() {
		t.Errorf("agent running at the pass %v, stopped %v; want it stopped first", running.Load(), sb.agentKilled.Load())
	}
}

// unreadableFolderStore fails one of the facts a pass back reads.
type unreadableFolderStore struct {
	*computerFolderStore
	seedErr, statusErr error
}

func (s *unreadableFolderStore) RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	if s.seedErr != nil {
		return nil, store.LocalFolderSeed{}, s.seedErr
	}
	return s.computerFolderStore.RunLocalFolderSeed(ctx, id)
}

func (s *unreadableFolderStore) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if s.statusErr != nil {
		return types.RunLocalFolderStatus{}, s.statusErr
	}
	return s.computerFolderStore.RunLocalFolderStatus(ctx, id)
}

// A fact that cannot be read keeps the sandbox, contained with its proxy stopped, and writes no record:
// the run.folder.transfer failure row names the fact.
func TestPass_UnreadableFactsContainAndKeep(t *testing.T) {
	for name, us := range map[string]unreadableFolderStore{
		"seed":   {seedErr: errors.New("seed read failed")},
		"status": {statusErr: errors.New("status read failed")},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatal(cs.FailureHint())
			}
			us.computerFolderStore = cs
			s.cfg.Store = &us
			seen := passSeen(sb)
			if !bringBackHeld(t, s, run.ID, "sbx") || seen.Load() || !sb.proxyStopped.Load() {
				t.Errorf("pass %v, proxy stopped %v; want kept, contained, with no pass", seen.Load(), sb.proxyStopped.Load())
			}
			if st := cs.folderStatus(); st.Back != nil || st.Gen != 0 {
				t.Errorf("record %+v, want none written", st)
			}
			ev := findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), "folder_record_error") {
				t.Errorf("row %+v, want a run.folder.transfer failure with folder_record_error", ev)
			}
		})
	}
}

// A pass that completed but left changed files it could not bring back still owes them: it is kept for a
// resume, as a cut pass is, and not recorded as done.
func TestFolderBackVerdict_ACompletedPassWithUnsyncedFilesIsOwed(t *testing.T) {
	next, errText := folderBackVerdict(nil, &runnerwire.FolderPass{Pulled: 1, Unsynced: 2, UnsyncedPaths: []string{"a.txt", "b.txt"}}, "", false, nil)
	if next == nil || next.Unsynced != 2 || !strings.Contains(errText, "conflict copy") {
		t.Errorf("verdict = %+v, %q; want a resume owing 2 files", next, errText)
	}
	if next, _ := folderBackVerdict(nil, &runnerwire.FolderPass{Pulled: 1}, "", false, nil); next != nil {
		t.Errorf("a clean completed pass left a resume: %+v", next)
	}
}
