// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	// The cap is the database's answer (store.RunDue.CapPassed, dueFlagsAt standing for it here), never this server's
	// clock: the same run at the same server time reads the same either side of the database's cap.
	for name, c := range map[string]struct {
		now           time.Time
		expired, kept bool
	}{
		"a second before the cap": {end.Add(s.pendingCap() - time.Second), false, true},
		"at the cap":              {end.Add(s.pendingCap()), true, false},
		"a second past":           {end.Add(s.pendingCap() + time.Second), true, false},
	} {
		due := dueFlagsAt(run, s.pendingCap(), c.now)
		expired, err1 := s.folderExpiredOwed(context.Background(), run, st, due)
		kept, err2 := s.folderKeepPermitted(context.Background(), run, st, due)
		if err1 != nil || err2 != nil || expired != c.expired || kept != c.kept {
			t.Errorf("%s: expired %v kept %v (%v, %v); want %v and %v", name, expired, kept, err1, err2, c.expired, c.kept)
		}
	}
	run.UpdatedAt, run.EndedAt = end, nil
	if expired, _ := s.folderExpiredOwed(context.Background(), run, st, dueFlagsAt(run, s.pendingCap(), end.Add(s.pendingCap()))); !expired {
		t.Error("a run with no ended_at is not timed from its last update")
	}
}

func folderFateFor(t *testing.T, run types.AgentRun, st types.RunLocalFolderStatus, seeded, owed bool) folderFate {
	t.Helper()
	return folderFateOf(run, st, seeded, owed, 7*24*time.Hour)
}

func fateRun(state types.RunState, ref string) types.AgentRun {
	end := time.Now().Add(-time.Hour).UTC()
	return types.AgentRun{ID: uuid.New(), State: state, SandboxRef: ref, EndedAt: &end}
}

func deref(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}

// A terminal run that owes its copy back is retained, with its sandbox kept, the last report's count and
// names, and the day the keep ends. The same fields come out of the run view.
func TestFolderFate_RetainedWhileOwed(t *testing.T) {
	run := fateRun(types.RunStopped, "sbx")
	f := folderFateFor(t, run, owedStatus(), true, true)
	if deref(f.fate) != "retained" || deref(f.sandboxCopy) != "kept" || f.lastReport == nil || f.lastReport.Unsynced == nil || *f.lastReport.Unsynced != 3 ||
		len(f.lastReport.Paths) != 3 || f.lastReport.PathsMore != 0 || f.giveUpAt == nil || !f.giveUpAt.Equal(run.EndedAt.Add(7*24*time.Hour)) {
		t.Fatalf("fate = %s %s %+v %v; want retained, kept, 3 changes named, the keep's end", deref(f.fate), deref(f.sandboxCopy), f.lastReport, f.giveUpAt)
	}

	// More changes than names: the rest are counted.
	st := owedStatus()
	st.Resume.Unsynced = 80
	st.Resume.Paths = make([]string, 60)
	f = folderFateFor(t, run, st, true, true)
	if len(f.lastReport.Paths) != runnerwire.MaxFolderUnsyncedPaths || f.lastReport.PathsMore != 30 || *f.lastReport.Unsynced != 80 {
		t.Errorf("report = %+v, want 50 names and 30 more of 80", f.lastReport)
	}

	// Through the run view, from the store.
	s, cs, _, vrun := giveUpFixture(t)
	v := s.computerFolderRunView(context.Background(), vrun.ID)
	if v == nil || deref(v.Fate) != "retained" || deref(v.SandboxCopy) != "kept" || v.LastReport == nil || v.GiveUpAt == nil {
		t.Fatalf("run view = %+v, want the retained fate", v)
	}
	cs.dataOut = types.TransferBlocked
	if v := s.computerFolderRunView(context.Background(), vrun.ID); v == nil || v.Fate != nil {
		t.Errorf("a folder whose data_out rule keeps its content in has fate %v, want none: nothing is owed", v)
	}
}

// A record given up while its stop is not yet confirmed: given up, and still kept, with its own report and
// no claim that the copy is gone.
func TestFolderFate_GivenUpButKept(t *testing.T) {
	at := time.Now().UTC()
	n := 4
	st := types.RunLocalFolderStatus{Gen: 8, Back: &types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}, Error: "given up",
		GivenUpAt: &at, GivenUpCount: &n, GivenUpPaths: []string{"a.txt", "b.txt"}, GivenUpPathsMore: 2}}
	f := folderFateFor(t, fateRun(types.RunStopped, "sbx"), st, true, false)
	if deref(f.fate) != "given_up" || deref(f.sandboxCopy) != "kept" || f.lastReport == nil || *f.lastReport.Unsynced != 4 ||
		len(f.lastReport.Paths) != 2 || f.lastReport.PathsMore != 2 || f.giveUpAt != nil {
		t.Errorf("fate = %s %s %+v %v; want given_up, kept, the give-up's own report", deref(f.fate), deref(f.sandboxCopy), f.lastReport, f.giveUpAt)
	}
}

// Deleted is said for a terminal run whose ref is clear and which had a sandbox, and for nothing else.
func TestFolderFate_DeletedOnlyAfterConfirmedTeardown(t *testing.T) {
	done := types.RunLocalFolderStatus{Gen: 3, Back: &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}}}
	for name, c := range map[string]struct {
		run    types.AgentRun
		st     types.RunLocalFolderStatus
		seeded bool
		want   string
	}{
		"ended, ref clear, seeded":             {fateRun(types.RunStopped, ""), done, true, "deleted"},
		"ended, ref clear, never seeded":       {fateRun(types.RunFailed, ""), types.RunLocalFolderStatus{}, false, "<absent>"},
		"ended, ref still set":                 {fateRun(types.RunStopped, "sbx"), done, true, "kept"},
		"ended, owed, ref still set":           {fateRun(types.RunStopped, "sbx"), owedStatus(), true, "kept"},
		"live, ref clear":                      {fateRun(types.RunRunning, ""), types.RunLocalFolderStatus{}, true, "<absent>"},
		"live, ref set":                        {fateRun(types.RunRunning, "sbx"), types.RunLocalFolderStatus{}, true, "<absent>"},
		"given up, ref clear (stop confirmed)": {fateRun(types.RunStopped, ""), types.RunLocalFolderStatus{Back: &types.FolderPassRecord{GivenUpAt: &time.Time{}}}, true, "deleted"},
	} {
		f := folderFateFor(t, c.run, c.st, c.seeded, false)
		if got := deref(f.sandboxCopy); got != c.want {
			t.Errorf("%s: sandbox_copy = %s, want %s", name, got, c.want)
		}
	}
}

// A count with no names keeps its count and has no list; a count nobody made is null, never zero.
func TestFolderFate_CountOnlyKeepsCount(t *testing.T) {
	run := fateRun(types.RunStopped, "sbx")
	back := &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}, Error: "cut"}
	for name, c := range map[string]struct {
		resume *types.FolderResume
		want   string
	}{
		"count, no names":             {&types.FolderResume{Stalled: 1, Unsynced: 5, Counted: true}, `"unsynced":5`},
		"a count from before Counted": {&types.FolderResume{Stalled: 1, Unsynced: 5}, `"unsynced":5`},
		"never counted":               {&types.FolderResume{Stalled: 1}, `"unsynced":null`},
	} {
		f := folderFateFor(t, run, types.RunLocalFolderStatus{Gen: 2, Resume: c.resume, Back: back}, true, true)
		b, err := json.Marshal(f.lastReport)
		if err != nil || !strings.Contains(string(b), c.want) || strings.Contains(string(b), `"paths`) {
			t.Errorf("%s: last_report = %s (%v), want %s and no list", name, b, err, c.want)
		}
	}
	at := time.Now().UTC()
	given := folderFateFor(t, run, types.RunLocalFolderStatus{Gen: 8, Back: &types.FolderPassRecord{At: at, GivenUpAt: &at, Refused: []types.FolderRefusal{}}}, true, false)
	if b, _ := json.Marshal(given.lastReport); !strings.Contains(string(b), `"unsynced":null`) {
		t.Errorf("a give-up of changes nobody counted reports %s, want unsynced null", b)
	}
}

func TestFolderFate_AbsentWhenLive(t *testing.T) {
	f := folderFateFor(t, fateRun(types.RunRunning, "sbx"), owedStatus(), true, true)
	if f.fate != nil || f.sandboxCopy != nil || f.lastReport != nil || f.giveUpAt != nil {
		t.Errorf("a live run has fate %s, copy %s, report %+v, give-up %v; want none", deref(f.fate), deref(f.sandboxCopy), f.lastReport, f.giveUpAt)
	}
	// A done record is complete only when its pass ended without an error.
	run := fateRun(types.RunStopped, "")
	ok := types.RunLocalFolderStatus{Gen: 2, Back: &types.FolderPassRecord{At: time.Now().UTC(), Refused: []types.FolderRefusal{}}}
	if f := folderFateFor(t, run, ok, true, false); deref(f.fate) != "complete" {
		t.Errorf("a completed pass has fate %s, want complete", deref(f.fate))
	}
	ok.Back.Error = "the sandbox was stopped with changed files not brought back: 1"
	if f := folderFateFor(t, run, ok, true, false); f.fate != nil {
		t.Errorf("a pass that ended in an error has fate %s, want none: the server cannot say it is complete", deref(f.fate))
	}
}
