// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	auditlog "github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// owedStatus is a record an owner holds at generation 7: a Resume of three counted changes, three names.
func owedStatus() types.RunLocalFolderStatus {
	return types.RunLocalFolderStatus{
		Gen: 7, Settled: 7,
		Resume: &types.FolderResume{Stalled: 1, Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b/c.txt", "d.txt"}},
		Back:   &types.FolderPassRecord{At: time.Now().UTC(), Files: 1, Refused: []types.FolderRefusal{}, Error: "the sync was cut short"},
	}
}

// giveUpFixture is a STOPPED run that ended past its keep and still holds "sbx", with a seeded both-ways
// folder and a record held by an owner at generation 7.
func giveUpFixture(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun) {
	t.Helper()
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	seeded := time.Now()
	cs.mu.Lock()
	cs.seed = store.LocalFolderSeed{SeededAt: &seeded}
	cs.mu.Unlock()
	end := time.Now().Add(-s.pendingCap() - time.Hour)
	run.State, run.SandboxRef, run.EndedAt = types.RunStopped, "sbx", &end
	cs.dispatchTestStore.mu.Lock()
	cs.run.EndedAt, cs.run.SandboxRef, cs.state = &end, "sbx", types.RunStopped
	cs.dispatchTestStore.mu.Unlock()
	cs.smu.Lock()
	cs.status = owedStatus()
	cs.smu.Unlock()
	return s, cs, audit, run
}

func (s *computerFolderStore) giveUpCalls() []store.GiveUpParams {
	s.smu.Lock()
	defer s.smu.Unlock()
	return append([]store.GiveUpParams(nil), s.giveUps...)
}

func (s *computerFolderStore) markCalls() []int {
	s.smu.Lock()
	defer s.smu.Unlock()
	return append([]int(nil), s.marks...)
}

// transferRows is the run.folder.transfer rows the audit log holds for run.
func transferRows(audit *recRecorder, run types.AgentRun) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Action == "run.folder.transfer" {
			out = append(out, ev)
		}
	}
	return out
}

// A statement that applied to nothing is not final unless the record is: the run may not be due, or its
// record still open, and stopping its sandbox then loses the changes.
func TestGiveUp_ZeroRowsNotDoneIsNotFinal(t *testing.T) {
	s, cs, audit, run := giveUpFixture(t)
	final, err := s.giveUpFolder(context.Background(), run, false, cs.folderStatus())
	if final || err != nil {
		t.Fatalf("giveUpFolder = %v, %v; want not final and no error", final, err)
	}
	if n := len(cs.giveUpCalls()); n != 1 {
		t.Errorf("the statement ran %d times, want 1", n)
	}
	if rows := transferRows(audit, run); len(rows) != 0 {
		t.Errorf("a give-up that applied to nothing wrote %d audit rows", len(rows))
	}
	if st := cs.folderStatus(); st.Gen != 7 || st.Resume == nil {
		t.Errorf("record = %+v, want it untouched", st)
	}
}

func TestGiveUp_ZeroRowsButDoneIsFinal(t *testing.T) {
	s, cs, audit, run := giveUpFixture(t)
	cs.smu.Lock()
	cs.status = types.RunLocalFolderStatus{Gen: 9, Back: &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}}}
	cs.smu.Unlock()
	final, err := s.giveUpFolder(context.Background(), run, true, cs.folderStatus())
	if !final || err != nil {
		t.Fatalf("giveUpFolder = %v, %v; want final: the record is done from another writer", final, err)
	}
	if rows := transferRows(audit, run); len(rows) != 0 {
		t.Errorf("a give-up that applied to nothing wrote %d audit rows", len(rows))
	}
}

func TestGiveUp_StoreErrorIsNotFinal(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	cs.giveUpErr = errors.New("database down")
	if final, err := s.giveUpFolder(context.Background(), run, false, cs.folderStatus()); final || err == nil {
		t.Fatalf("giveUpFolder = %v, %v; want an error and not final", final, err)
	}
}

// failingAudit is an audit log that takes nothing: it neither writes nor spools.
type failingAudit struct{ err error }

func (f failingAudit) Record(context.Context, types.AuditEvent) error { return f.err }

// An applied give-up is final at once, whatever the audit log answers. Its row is written, and the record
// marked, only when the row was accepted (written, or held by the audit spool).
func TestGiveUp_RowMarkedAudited(t *testing.T) {
	ctx := context.Background()
	s, cs, audit, run := giveUpFixture(t)
	cs.giveUpApplies = true
	final, err := s.giveUpFolder(ctx, run, false, cs.folderStatus())
	if !final || err != nil {
		t.Fatalf("giveUpFolder = %v, %v; want final", final, err)
	}
	rows := transferRows(audit, run)
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"direction":"back"`) ||
		!strings.Contains(string(rows[0].Data), "were given up when the sandbox's keep ended: changed files not brought back: 3") {
		t.Fatalf("audit rows = %+v, want one failure row naming the give-up and its count", rows)
	}
	st := cs.folderStatus()
	if st.Gen != 8 || st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil || !st.Back.GivenUpAudited ||
		st.Back.GivenUpCount == nil || *st.Back.GivenUpCount != 3 || len(st.Back.GivenUpPaths) != 3 || st.Back.GivenUpPathsMore != 0 {
		t.Errorf("record = %+v, want gen 8, given up with 3 names, audited", st)
	}
	if m := cs.markCalls(); len(m) != 1 || m[0] != 8 {
		t.Errorf("marks at generations %v, want one, at 8", m)
	}
	if calls := cs.giveUpCalls(); len(calls) != 1 || calls[0].Cap != s.pendingCap() || calls[0].D1 {
		t.Errorf("statement calls = %+v, want one with the pending cap and no D1", calls)
	}

	for name, c := range map[string]struct {
		setup      func(*Server, *computerFolderStore)
		wantMarked bool
		wantLog    string
		markCalled bool
	}{
		"the mark fails": {func(_ *Server, cs *computerFolderStore) { cs.markErr = errors.New("write failed") }, false, "write failed", true},
		"the audit log takes nothing": {func(s *Server, _ *computerFolderStore) {
			s.cfg.Audit = failingAudit{errors.New("audit down")}
		}, false, "did not take the row", false},
		"the audit log spools the row": {func(s *Server, _ *computerFolderStore) {
			s.cfg.Audit = failingAudit{fmt.Errorf("store down: %w", auditlog.ErrSpooled)}
		}, true, "", true},
	} {
		s, cs, _, run := giveUpFixture(t)
		cs.giveUpApplies = true
		c.setup(s, cs)
		var logs bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		final, err := s.giveUpFolder(ctx, run, false, cs.folderStatus())
		slog.SetDefault(old)
		if !final || err != nil {
			t.Errorf("%s: giveUpFolder = %v, %v; want final whatever the audit answers", name, final, err)
		}
		st := cs.folderStatus()
		if st.Back == nil || st.Back.GivenUpAt == nil || st.Back.GivenUpAudited != c.wantMarked {
			t.Errorf("%s: record %+v, want given up and audited=%v", name, st.Back, c.wantMarked)
		}
		if c.wantLog != "" && !strings.Contains(logs.String(), c.wantLog) {
			t.Errorf("%s: log %q does not name %q", name, logs.String(), c.wantLog)
		}
		if called := len(cs.markCalls()) != 0; called != c.markCalled {
			t.Errorf("%s: mark called = %v, want %v", name, called, c.markCalled)
		}
	}
}

// A record given up is final: no pass runs for it, and the sandbox is stopped.
func TestFolderBack_GivenUpRecordStops(t *testing.T) {
	s, cs, audit, run := giveUpFixture(t)
	at := time.Now().UTC()
	n := 3
	cs.smu.Lock()
	cs.status = types.RunLocalFolderStatus{Gen: 8, Back: &types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}, Error: "given up", GivenUpAt: &at, GivenUpCount: &n}}
	cs.smu.Unlock()
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Fatal("bringComputerFolderBack kept the sandbox of a record given up")
	}
	if st := cs.folderStatus(); st.Gen != 8 || st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil {
		t.Errorf("record = %+v, want it untouched: no pass moved it on", st)
	}
	if rows := transferRows(audit, run); len(rows) != 0 {
		t.Errorf("a pass ran for a record given up: %d transfer rows", len(rows))
	}
}

// A pass that owned generation 7 and returns after the give-up made 8 writes nothing.
func TestFolderBack_GivenUpRecordIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	s, cs, audit, run := giveUpFixture(t)
	cs.giveUpApplies = true
	if final, err := s.giveUpFolder(ctx, run, false, cs.folderStatus()); !final || err != nil {
		t.Fatalf("giveUpFolder = %v, %v", final, err)
	}
	folder, _, err := cs.RunLocalFolderSeed(ctx, run.ID)
	if err != nil || folder == nil {
		t.Fatalf("folder: %v %v", folder, err)
	}
	s.recordFolderBackAt(ctx, run.ID, *folder, 7, true, nil, runnerwire.FolderPass{}, "")
	st := cs.folderStatus()
	if st.Gen != 8 || st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil || !strings.Contains(st.Back.Error, "were given up") {
		t.Errorf("record = %+v, want the give-up kept at gen 8", st)
	}
	if rows := transferRows(audit, run); len(rows) != 1 {
		t.Errorf("%d transfer rows, want only the give-up's: a pass that lost its generation audits nothing", len(rows))
	}
}

// Killing a run doesn't bring the folder back: a KILLED run that reaches the pass, as when the kill's stop failed
// and a later owner takes the tail, is stopped with no pass and no write.
func TestFolderBack_KilledRunIsNeverCopiedBack(t *testing.T) {
	s, cs, audit, run := giveUpFixture(t)
	cs.dispatchTestStore.mu.Lock()
	cs.state = types.RunKilled
	cs.dispatchTestStore.mu.Unlock()
	if s.bringComputerFolderBack(context.Background(), run.ID, "sbx") {
		t.Fatal("bringComputerFolderBack kept the sandbox of a killed run")
	}
	if st := cs.folderStatus(); st.Gen != 7 || st.Resume == nil || st.Back == nil || st.Back.GivenUpAt != nil {
		t.Errorf("record = %+v, want it untouched: no pass moved it on", st)
	}
	if rows := transferRows(audit, run); len(rows) != 0 {
		t.Errorf("a pass ran for a killed run: %d transfer rows", len(rows))
	}
}

func TestFolderOwed_KilledIsNeverOwed(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	st := cs.folderStatus()
	for state, want := range map[types.RunState]bool{types.RunKilled: false, types.RunStopped: true} {
		run.State = state
		if got, err := s.folderOwed(context.Background(), run, st); err != nil || got != want {
			t.Errorf("%s run with a ref, a seeded both-ways folder and a Resume: owed = %v, %v; want %v", state, got, err, want)
		}
	}
}

func TestFolderOwed_IneligibleIsNotOwed(t *testing.T) {
	for name, breakIt := range map[string]func(*computerFolderStore){
		"no folder runner": func(cs *computerFolderStore) { cs.folder.RunnerID = nil },
		"one way":          func(cs *computerFolderStore) { cs.folder.Direction = types.ComputerFolderIn },
		"unseeded":         func(cs *computerFolderStore) { cs.seed = store.LocalFolderSeed{} },
		"data out denied":  func(cs *computerFolderStore) { cs.dataOut = types.TransferBlocked },
	} {
		s, cs, _, run := giveUpFixture(t)
		cs.mu.Lock()
		breakIt(cs)
		cs.mu.Unlock()
		if got, err := s.folderOwed(context.Background(), run, cs.folderStatus()); err != nil || got {
			t.Errorf("%s: owed = %v, %v; want not owed and no error", name, got, err)
		}
	}
}

// A read that fails is an error, never "not owed": the caller skips the row, and stopping it would lose the changes.
func TestFolderOwed_ReadErrorIsAnError(t *testing.T) {
	for name, breakIt := range map[string]func(*computerFolderStore){
		"the folder seed cannot be read":  func(cs *computerFolderStore) { cs.seedErr = errors.New("database down") },
		"the data out rule is unreadable": func(cs *computerFolderStore) { cs.policyErr = errors.New("audit store down") },
	} {
		s, cs, _, run := giveUpFixture(t)
		breakIt(cs)
		if got, err := s.folderOwed(context.Background(), run, cs.folderStatus()); err == nil || got {
			t.Errorf("%s: owed = %v, %v; want an error", name, got, err)
		}
		if got, err := s.folderBackEligible(context.Background(), run.ID); err == nil || got {
			t.Errorf("%s: eligible = %v, %v; want an error", name, got, err)
		}
	}
}

func TestPendingCap(t *testing.T) {
	s := &Server{}
	for grace, want := range map[time.Duration]time.Duration{
		0:                   7 * 24 * time.Hour,
		3 * 24 * time.Hour:  7 * 24 * time.Hour,
		7 * 24 * time.Hour:  7 * 24 * time.Hour,
		30 * 24 * time.Hour: 30 * 24 * time.Hour,
	} {
		s.cfg.EndedRunGrace = grace
		if got := s.pendingCap(); got != want {
			t.Errorf("grace %s: pendingCap = %s, want %s", grace, got, want)
		}
	}
}

func TestFolderKeepAndExpiry(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	st := cs.folderStatus()
	end := *run.EndedAt
	for name, c := range map[string]struct {
		now           time.Time
		expired, kept bool
	}{
		"a second before the cap": {end.Add(s.pendingCap() - time.Second), false, true},
		"at the cap":              {end.Add(s.pendingCap()), true, false},
		"a second past":           {end.Add(s.pendingCap() + time.Second), true, false},
	} {
		expired, err1 := s.folderExpiredOwed(context.Background(), run, st, c.now)
		kept, err2 := s.folderKeepPermitted(context.Background(), run, st, c.now)
		if err1 != nil || err2 != nil || expired != c.expired || kept != c.kept {
			t.Errorf("%s: expired %v kept %v (%v, %v); want %v and %v", name, expired, kept, err1, err2, c.expired, c.kept)
		}
	}
	run.UpdatedAt, run.EndedAt = end, nil
	if expired, _ := s.folderExpiredOwed(context.Background(), run, st, end.Add(s.pendingCap())); !expired {
		t.Error("a run with no ended_at is not timed from its last update")
	}
}
