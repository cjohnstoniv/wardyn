// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// owedFixture is computerFolderFixture whose runner, of this release over the real wire, keeps its state
// under state and, once armed, has its account of each pass edited by edit. It is not yet held.
type owedFixture struct {
	s     *Server
	cs    *computerFolderStore
	audit *recRecorder
	run   types.AgentRun
	sb    *folderSandboxRunner
	app   string
	state string
	peer  *runnerwire.Peer
	armed atomic.Bool
}

func newOwedFixture(t *testing.T, edit func(*runnerwire.FolderPass)) *owedFixture {
	t.Helper()
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	f := &owedFixture{s: s, cs: cs, audit: audit, run: run, sb: sb, app: app, state: t.TempDir()}
	o := folderRunnerOpts{caps: onePerRunnerCaps, stateDir: f.state}
	if edit != nil {
		o.answer = func(method string, raw json.RawMessage) json.RawMessage {
			var p runnerwire.FolderPass
			if method != runnerwire.MethodSyncFolderWait || !f.armed.Load() || json.Unmarshal(raw, &p) != nil {
				return raw
			}
			edit(&p)
			out, err := json.Marshal(p)
			if err != nil {
				return raw
			}
			return out
		}
	}
	ep, peer := folderRunnerEndpointWith(t, cs.runner.ID, o, filepath.Dir(filepath.Dir(app)))
	s.runnerStreams.subsMu.Lock()
	s.runnerStreams.subs[cs.runner.ID] = ep
	s.runnerStreams.subsMu.Unlock()
	f.peer = peer
	return f
}

// hold seeds the folder, then arms the account edit.
func (f *owedFixture) hold(t *testing.T) {
	t.Helper()
	if !f.s.holdForLocalFolder(context.Background(), f.run, "sbx") {
		t.Fatal(f.cs.FailureHint())
	}
	f.armed.Store(true)
}

// finalize is one visit of the run's end, through finalizeRunEffects.
func (f *owedFixture) finalize(t *testing.T) error {
	t.Helper()
	_, err := boundedFinalize(t, f.s, f.run, time.Second)
	return err
}

// stateFile is the runner's sync state for the run.
func (f *owedFixture) stateFile() string {
	return filepath.Join(f.state, "folders", f.run.ID.String()+".json")
}

// conflictName is the n-th name a sandbox copy of base+ext may take on the laptop.
func (f *owedFixture) conflictName(base, ext string, n int) string {
	tag := f.run.ID.String()[:8]
	if n > 1 {
		tag += fmt.Sprintf(" %d", n)
	}
	return filepath.Join(f.app, base+" (sandbox conflict "+tag+")"+ext)
}

// assertKeptOwing checks a visit that kept the sandbox for a later pass, recording an owed outcome naming
// each of want, and stopping nothing.
func (f *owedFixture) assertKeptOwing(t *testing.T, err error, want ...string) types.FolderResume {
	t.Helper()
	if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
		t.Fatalf("finalize = %v, stops %d; want the sandbox kept", err, f.sb.stopCount())
	}
	st := f.cs.folderStatus()
	if st.Resume == nil || st.Back == nil || st.Back.Error == "" {
		t.Fatalf("record %+v, want a resume and an owed outcome", st)
	}
	for _, w := range append(want, "a later sweep, about hourly, tries the sync again") {
		if !strings.Contains(st.Back.Error, w) {
			t.Errorf("back record %q, want it naming %q", st.Back.Error, w)
		}
		if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", w); ev == nil || ev.Outcome != "failure" {
			t.Errorf("no failure row naming %q: %v", w, ev)
		}
	}
	return *st.Resume
}

// assertTornDownClean checks a visit that completed and stopped the sandbox once, with nothing owed.
func (f *owedFixture) assertTornDownClean(t *testing.T, err error) {
	t.Helper()
	if err != nil || f.sb.stopCount() != 1 {
		t.Fatalf("finalize = %v, stops %d; want one stop", err, f.sb.stopCount())
	}
	if st := f.cs.folderStatus(); st.Back == nil || st.Back.Error != "" || st.Resume != nil {
		t.Errorf("record %+v, want a completed pass and no resume", st)
	}
}

// keptAfterStateSaveFails runs X1's first visit: the runner's state file turns into a non-empty directory
// after the pass loaded it and started, before the save.
func (f *owedFixture) keptAfterStateSaveFails(t *testing.T) {
	t.Helper()
	f.hold(t)
	agentWrites(t, f.sb, "a.txt")
	if _, err := os.Stat(f.stateFile()); err != nil {
		t.Fatalf("the seed saved no sync state: %v", err)
	}
	var broke atomic.Bool
	f.sb.onRead = func(name string) {
		// The read of a.txt is the pass under way: its state was loaded, and its save is still to come.
		if name != "a.txt" || broke.Swap(true) {
			return
		}
		if err := os.Remove(f.stateFile()); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(f.stateFile(), "held"), 0o755); err != nil {
			t.Error(err)
		}
	}
	f.assertKeptOwing(t, f.finalize(t), "could not confirm", "save sync state")
	f.sb.onRead = nil
	if !broke.Load() {
		t.Fatal("the pass never read a.txt")
	}
}

// X1. A pass back whose runner could not save its sync state keeps the sandbox, saying so; once the state
// can be saved, the next visit tears down, with no conflict copy.
func TestFinalizeRunEffects_AStateSaveFailureKeepsTheSandbox(t *testing.T) {
	f := newOwedFixture(t, nil)
	f.keptAfterStateSaveFails(t)
	if err := os.RemoveAll(f.stateFile()); err != nil {
		t.Fatal(err)
	}
	f.assertTornDownClean(t, f.finalize(t))
	_ = filepath.WalkDir(f.app, func(p string, _ fs.DirEntry, _ error) error {
		if strings.Contains(filepath.Base(p), "(sandbox conflict") {
			t.Errorf("conflict copy %s after a clean pass", p)
		}
		return nil
	})
}

// holdWithNoFreeCopy takes every conflict copy name of README.md on the laptop with other bytes, before the
// seed so that the pass back finds them synced, then holds and has the agent and the laptop both change
// README.md.
func (f *owedFixture) holdWithNoFreeCopy(t *testing.T) {
	t.Helper()
	for n := 1; n <= 9; n++ {
		if err := os.WriteFile(f.conflictName("README", ".md", n), []byte("taken"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.hold(t)
	agentWrites(t, f.sb, "README.md")
	if err := os.WriteFile(filepath.Join(f.app, "README.md"), []byte("hello from the laptop"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// X2. A conflict whose sandbox copy cannot be kept keeps the sandbox, a stall; the second such visit gives
// the change up, and only then is the sandbox stopped.
func TestFinalizeRunEffects_AConflictCopyThatCannotBeKeptGivesUpAfterTwoStalls(t *testing.T) {
	f := newOwedFixture(t, nil)
	f.holdWithNoFreeCopy(t)
	head := "the pass back could not confirm 1 changed files kept on the laptop, so they count as not copied back: keep the sandbox copy of README.md: not kept"
	want := types.FolderResume{Stalled: 1, Unsynced: 1, Counted: true, Paths: []string{"README.md"}}
	if r := f.assertKeptOwing(t, f.finalize(t), head); !reflect.DeepEqual(r, want) {
		t.Errorf("resume %+v, want %+v: one stall, one counted change left, named README.md", r, want)
	}
	if err := f.finalize(t); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "the sandbox was stopped with changed files not brought back: 1, because 2 passes in a row brought nothing back: "+head)
}

// X3. A file the laptop deleted while the agent changed it comes back as a conflict copy with the agent's
// bytes, and the pass completes.
func TestFinalizeRunEffects_ALaptopDeleteOfAnAgentChangeKeepsAConflictCopy(t *testing.T) {
	f := newOwedFixture(t, nil)
	if err := os.WriteFile(filepath.Join(f.app, "a.txt"), []byte("a.txt by the laptop"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.hold(t)
	agentWrites(t, f.sb, "a.txt")
	if err := os.Remove(filepath.Join(f.app, "a.txt")); err != nil {
		t.Fatal(err)
	}
	f.assertTornDownClean(t, f.finalize(t))
	if b, err := os.ReadFile(f.conflictName("a", ".txt", 1)); err != nil || string(b) != "a.txt by the agent" {
		t.Errorf("conflict copy = %q %v, want the agent's bytes", b, err)
	}
}

// X4. A resume visit whose runner is gone, and staying gone, sends no pass: nothing is counted, so it is no
// stall, and the sandbox stays kept, visit after visit, with the stalls and changes it already had.
func TestFinalizeRunEffects_AResumeWhoseRunnerIsGoneKeepsTheSandboxUncounted(t *testing.T) {
	f := newOwedFixture(t, nil)
	f.keptAfterStateSaveFails(t)
	before := *f.cs.folderStatus().Resume
	f.peer.Close()
	f.s.cfg.RunnerOnline = func(id uuid.UUID) bool { return id != f.cs.runner.ID }
	for visit := 1; visit <= 2; visit++ {
		err := f.finalize(t)
		if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
			t.Fatalf("visit %d: finalize = %v, stops %d; want the sandbox kept", visit, err, f.sb.stopCount())
		}
		st := f.cs.folderStatus()
		if st.Resume == nil || *st.Resume != before || st.Settled != st.Gen ||
			st.Back == nil || st.Back.Error != folderPassAwayMsg {
			t.Fatalf("visit %d: record %+v, want the resume as it was, settled, with the runner-offline text", visit, st)
		}
	}
}

// X5. A runner that does not say whether it kept a change (one from before not_preserved) and completes
// with a change left behind is kept, never recorded as done.
func TestFinalizeRunEffects_ACompletedPassThatLeftAChangeBehindIsKept(t *testing.T) {
	f := newOwedFixture(t, func(p *runnerwire.FolderPass) { p.NotPreserved = "" })
	f.holdWithNoFreeCopy(t)
	f.assertKeptOwing(t, f.finalize(t), "the pass back left 1 changed files not brought back: a file changed on both sides could not be kept as a conflict copy")
	if st := f.cs.folderStatus(); st.Back == nil || st.Back.Error == "" {
		t.Errorf("record %+v, want the change left behind recorded, not a completed pass", st)
	}
}

// X6. A change the laptop could not install (its folder read-only) keeps the sandbox; once it can, the next
// visit brings it back and tears down.
func TestFinalizeRunEffects_AnInstallFailureKeepsTheSandbox(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	f := newOwedFixture(t, nil)
	f.hold(t)
	agentWrites(t, f.sb, "new.txt")
	if err := os.Chmod(f.app, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.app, 0o755) })
	f.assertKeptOwing(t, f.finalize(t), "could not confirm 1 changed files kept on the laptop")
	if err := os.Chmod(f.app, 0o755); err != nil {
		t.Fatal(err)
	}
	f.assertTornDownClean(t, f.finalize(t))
	if b, err := os.ReadFile(filepath.Join(f.app, "new.txt")); err != nil || string(b) != "new.txt by the agent" {
		t.Errorf("new.txt on the laptop = %q %v, want the agent's", b, err)
	}
}

// An account the runner returned in time that names an error and a change it could not prove kept is owed:
// the sandbox is kept for a resume, never settled as a first failure.
func TestFinalizeRunEffects_AnAccountWithAnErrorAndAChangeLeftBehindIsOwed(t *testing.T) {
	f := newOwedFixture(t, func(p *runnerwire.FolderPass) {
		p.Error, p.Unsynced, p.NotPreserved = "unexpected EOF", 1, "install a.txt: unexpected EOF"
	})
	f.hold(t)
	agentWrites(t, f.sb, "a.txt")
	f.assertKeptOwing(t, f.finalize(t), "could not confirm 1 changed files kept on the laptop", "install a.txt: unexpected EOF")
}

// An account that names an error and a change left behind, having moved nothing, is a stall: kept with the
// stall counted, nothing stopped; the second in a row gives up.
func TestFinalizeRunEffects_AnErrorThatLeftAChangeAndMovedNothingIsAStall(t *testing.T) {
	f := newOwedFixture(t, func(p *runnerwire.FolderPass) {
		p.Error, p.Unsynced, p.Pulled, p.Pushed, p.NotPreserved = "unexpected EOF", 1, 0, 0, ""
	})
	f.hold(t)
	agentWrites(t, f.sb, "a.txt")
	want := types.FolderResume{Stalled: 1, Unsynced: 1, Counted: true}
	if r := f.assertKeptOwing(t, f.finalize(t), "unexpected EOF"); !reflect.DeepEqual(r, want) {
		t.Errorf("resume %+v, want %+v: one stall, one counted change left, no names", r, want)
	}
	if err := f.finalize(t); err != nil {
		t.Errorf("finalize: %v", err)
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, "the sandbox was stopped with changed files not brought back: 1, because 2 passes in a row brought nothing back: unexpected EOF")
}
