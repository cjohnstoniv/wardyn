// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refAtEndGiveUpStore is refStore noting the run's ref as each give-up is made.
type refAtEndGiveUpStore struct {
	refStore
	refAt *[]string
}

func (s refAtEndGiveUpStore) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	s.dispatchTestStore.mu.Lock()
	*s.refAt = append(*s.refAt, s.run.SandboxRef)
	s.dispatchTestStore.mu.Unlock()
	return s.refStore.GiveUpRunLocalFolder(ctx, id, p)
}

// endGiveUpFixture is computerFolderFixture's both-ways run, seeded, then ended in state with its sandbox "sbx" and
// rec as its folder record, on a store that keeps the ref and notes it at each give-up.
func endGiveUpFixture(t *testing.T, state types.RunState, rec types.RunLocalFolderStatus) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *folderSandboxRunner, *[]string) {
	t.Helper()
	s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.dispatchTestStore.mu.Lock()
	cs.run.SandboxRef, cs.state = "sbx", state
	cs.dispatchTestStore.mu.Unlock()
	cs.smu.Lock()
	cs.status.Gen, cs.status.Settled, cs.status.Resume, cs.status.Back = rec.Gen, rec.Settled, rec.Resume, rec.Back
	cs.smu.Unlock()
	refAt := []string{}
	s.cfg.Store = refAtEndGiveUpStore{refStore: refStore{cs}, refAt: &refAt}
	return s, cs, audit, run, sb, &refAt
}

// endGiveUpRows counts the run's run.folder.transfer failure rows naming want.
func endGiveUpRows(audit *recRecorder, runID uuid.UUID, want string) int {
	n := 0
	for _, ev := range audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == runID && ev.Action == "run.folder.transfer" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), want) {
			n++
		}
	}
	return n
}

// keptForCopyBack is a record kept for a resume, with 3 changes counted and 2 named.
func keptForCopyBack() types.RunLocalFolderStatus {
	return types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b.txt"}}}
}

// A run's end tail that stops a sandbox whose copy back is still owed (its pass back returned on a read that
// failed, so nothing was brought back or recorded) gives the copy back up before it clears the run's ref: made while
// the ref is still set, due at once (a zero cap), its final record keeps the count and names of the changes left
// behind, and its run.folder.transfer row is written; then the ref is cleared. A give-up that leaves the record
// open, or a read the give-up needs that fails, keeps the ref and answers an error. A killed run owes nothing: its
// ref is cleared with no give-up. Catches: a ref cleared while the record still shows the changes owed, with no
// given_up row; a read that fails answered as nothing owed.
func TestFinalizeRunEffects_GivesUpAnOwedCopyBackBeforeClearingTheRef(t *testing.T) {
	down := errors.New("the database did not answer")
	neverRun := types.RunLocalFolderStatus{Gen: 3, Settled: 3}
	for _, tc := range []struct {
		name               string
		state              types.RunState
		rec                types.RunLocalFolderStatus
		applies            bool
		seedErr, policyErr error
		heals              bool // the store answers again once the sandbox is stopped
		giveUps            int
		final              bool
	}{
		{"the folder seed could not be read", types.RunCompleted, keptForCopyBack(), true, down, nil, true, 1, true},
		{"the data out rule could not be read", types.RunCompleted, keptForCopyBack(), true, nil, down, true, 1, true},
		{"left open", types.RunCompleted, keptForCopyBack(), false, down, nil, true, 1, false},
		{"a read the give-up needs fails", types.RunCompleted, neverRun, true, down, nil, false, 0, false},
		{"a killed run owes nothing", types.RunKilled, neverRun, true, nil, nil, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, audit, run, sb, refAt := endGiveUpFixture(t, tc.state, tc.rec)
			cs.giveUpApplies, cs.seedErr, cs.policyErr = tc.applies, tc.seedErr, tc.policyErr
			sb.proxyErr = errors.New("the proxy did not stop") // the agent's cut fails, so no read error keeps the sandbox
			if tc.heals {
				sb.stopped = func() { cs.seedErr, cs.policyErr = nil, nil }
			}
			err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
			if sb.stopCount() != 1 {
				t.Fatalf("%d sandbox stops; want 1", sb.stopCount())
			}
			if len(cs.giveUps) != tc.giveUps || slices.ContainsFunc(*refAt, func(r string) bool { return r != "sbx" }) {
				t.Fatalf("%d give-ups, refs at give-up %v; want %d, each made while the ref was set", len(cs.giveUps), *refAt, tc.giveUps)
			}
			if tc.giveUps == 1 {
				p := cs.giveUps[0]
				if p.Cap != 0 || p.D1 || p.Back.GivenUpAt == nil || p.Back.GivenUpCount == nil || *p.Back.GivenUpCount != 3 ||
					!slices.Equal(p.Back.GivenUpPaths, []string{"a.txt", "b.txt"}) || p.Back.GivenUpPathsMore != 1 {
					t.Errorf("give-up %+v; want a zero cap, no containment terms, and 3 changes given up, 2 named and 1 more", p)
				}
			}
			wantRows := 0
			if tc.final && tc.giveUps == 1 {
				wantRows = 1
			}
			cur, _ := cs.GetRun(context.Background(), run.ID)
			rows := endGiveUpRows(audit, run.ID, "changed files not brought back: 3")
			if (err == nil) != tc.final || (cur.SandboxRef == "") != tc.final || rows != wantRows {
				t.Errorf("finalize = %v, ref %q, %d given_up rows; want an error %v, the ref cleared %v, %d rows",
					err, cur.SandboxRef, rows, !tc.final, tc.final, wantRows)
			}
		})
	}
}

// A sandbox kept for its copy back that its later pass back cannot quiesce (already stopped, as a removed container
// reads, or its agent will not stop) has the copy back given up with what its resume counted, before the end tail
// stops it and clears its ref. Catches: the count and names lost to a record that says uncounted, with no given_up
// row.
func TestFinalizeRunEffects_AKeptSandboxItCannotQuiesceIsGivenUpWithItsCount(t *testing.T) {
	for name, set := range map[string]func(*folderSandboxRunner){
		"its sandbox had already stopped": func(sb *folderSandboxRunner) { sb.ended = true },
		"its agent could not be stopped":  func(sb *folderSandboxRunner) { sb.killErr = errors.New("exec refused") },
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb, refAt := endGiveUpFixture(t, types.RunCompleted, keptForCopyBack())
			cs.giveUpApplies = true
			set(sb)
			if err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false); err != nil {
				t.Errorf("finalize = %v; want nil", err)
			}
			if len(cs.giveUps) != 1 || slices.ContainsFunc(*refAt, func(r string) bool { return r != "sbx" }) {
				t.Fatalf("%d give-ups, refs at give-up %v; want 1, made while the ref was set", len(cs.giveUps), *refAt)
			}
			if p := cs.giveUps[0]; p.Cap != 0 || p.Back.GivenUpCount == nil || *p.Back.GivenUpCount != 3 ||
				!slices.Equal(p.Back.GivenUpPaths, []string{"a.txt", "b.txt"}) {
				t.Errorf("give-up %+v; want a zero cap and 3 changes given up, 2 named", p)
			}
			st := cs.folderStatus()
			cur, _ := cs.GetRun(context.Background(), run.ID)
			if sb.stopCount() != 1 || st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil || cur.SandboxRef != "" {
				t.Errorf("%d stops, record %+v, ref %q; want one stop, the record given up, the ref cleared", sb.stopCount(), st, cur.SandboxRef)
			}
			if given, uncounted := endGiveUpRows(audit, run.ID, "changed files not brought back: 3"), endGiveUpRows(audit, run.ID, "uncounted"); given != 1 || uncounted != 0 {
				t.Errorf("%d given_up rows with the count, %d rows saying uncounted; want 1 and 0", given, uncounted)
			}
		})
	}
}

// unreadableAfterStopStore is endGiveUpFixture's store whose folder record cannot be read once down is set, and
// whose give-up answers Done for a record already final, as the statement reads it without decoding it.
type unreadableAfterStopStore struct {
	refAtEndGiveUpStore
	down *atomic.Bool
}

func (s unreadableAfterStopStore) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if s.down.Load() {
		return types.RunLocalFolderStatus{}, errors.New("the record does not decode")
	}
	return s.refAtEndGiveUpStore.RunLocalFolderStatus(ctx, id)
}

func (s unreadableAfterStopStore) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	out, err := s.refAtEndGiveUpStore.GiveUpRunLocalFolder(ctx, id, p)
	if err == nil && !out.Applied {
		s.smu.Lock()
		out.Done = folderDone(s.status)
		s.smu.Unlock()
	}
	return out, err
}

// A run's end tail whose folder record cannot be read once the sandbox is stopped decides it as the tail owner
// does: past the keep the copy back is given up here, noted that the record could not be read, or found already
// final, and the ref is cleared; inside the keep the ref is kept with an error; a killed run owes nothing and its
// ref is cleared with no give-up. A folder seed that cannot be read either changes none of that. Catches: a record
// that never reads keeping a stopped sandbox's ref for good, so that the tail owner's give-up past the keep stops the
// sandbox again on every tick.
func TestFinalizeRunEffects_AnUnreadableRecordIsDecidedAsTheTailOwnerDecidesIt(t *testing.T) {
	at := time.Now().UTC()
	givenUp := types.RunLocalFolderStatus{Gen: 4, Settled: 4, Back: &types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}, GivenUpAt: &at}}
	for _, tc := range []struct {
		name    string
		state   types.RunState
		rec     types.RunLocalFolderStatus
		applies bool // the run is past its keep
		giveUps int
		rows    int
		final   bool
		seedToo bool // the folder's seed stays unreadable as well
	}{
		{"past the keep, given up here", types.RunCompleted, keptForCopyBack(), true, 1, 1, true, false},
		{"past the keep, already given up by the tail owner", types.RunCompleted, givenUp, true, 1, 0, true, false},
		{"inside the keep", types.RunCompleted, keptForCopyBack(), false, 1, 0, false, false},
		{"a killed run owes nothing", types.RunKilled, keptForCopyBack(), true, 0, 0, true, false},
		{"past the keep, already given up, its seed unreadable too", types.RunCompleted, givenUp, true, 1, 0, true, true},
		{"inside the keep, its seed unreadable too", types.RunCompleted, keptForCopyBack(), false, 1, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, audit, run, sb, refAt := endGiveUpFixture(t, tc.state, tc.rec)
			down := &atomic.Bool{}
			s.cfg.Store = unreadableAfterStopStore{refAtEndGiveUpStore: s.cfg.Store.(refAtEndGiveUpStore), down: down}
			cs.giveUpApplies, cs.seedErr = tc.applies, errors.New("the database did not answer")
			sb.proxyErr = errors.New("the proxy did not stop") // the agent's cut fails, so no read error keeps the sandbox
			sb.stopped = func() {
				down.Store(true)
				if !tc.seedToo {
					cs.seedErr = nil
				}
			}
			err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
			if sb.stopCount() != 1 {
				t.Fatalf("%d sandbox stops; want 1", sb.stopCount())
			}
			if len(cs.giveUps) != tc.giveUps || slices.ContainsFunc(*refAt, func(r string) bool { return r != "sbx" }) {
				t.Fatalf("%d give-ups, refs at give-up %v; want %d, each made while the ref was set", len(cs.giveUps), *refAt, tc.giveUps)
			}
			for _, p := range cs.giveUps {
				if p.Cap != s.pendingCap() || p.D1 || p.Back.GivenUpCount != nil || !strings.HasSuffix(p.Back.Error, folderStatusUnreadable) {
					t.Errorf("give-up %+v; want the pending cap, no containment terms, nothing counted, and the unreadable record noted", p)
				}
			}
			cur, _ := cs.GetRun(context.Background(), run.ID)
			rows := endGiveUpRows(audit, run.ID, "status_unreadable")
			if (err == nil) != tc.final || (cur.SandboxRef == "") != tc.final || rows != tc.rows {
				t.Errorf("finalize = %v, ref %q, %d given_up rows; want an error %v, the ref cleared %v, %d rows",
					err, cur.SandboxRef, rows, !tc.final, tc.final, tc.rows)
			}
		})
	}
}

// A stop that fails gives nothing up: the sandbox still holds the changes, so the record keeps its resume and the
// run keeps its ref. Catches: a give-up made ahead of a stop that then fails, which leaves the record final while
// the sandbox still holds what it names.
func TestFinalizeRunEffects_AFailedStopGivesNothingUp(t *testing.T) {
	s, cs, _, run, sb, _ := endGiveUpFixture(t, types.RunCompleted, keptForCopyBack())
	cs.giveUpApplies, cs.seedErr = true, errors.New("the database did not answer")
	sb.proxyErr = errors.New("the proxy did not stop") // the agent's cut fails, so no read error keeps the sandbox
	stopper := &failingStopRunner{folderSandboxRunner: sb}
	s.cfg.Runner = stopper
	err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
	cur, _ := cs.GetRun(context.Background(), run.ID)
	if st := cs.folderStatus(); stopper.stops.Load() == 0 || err == nil || len(cs.giveUps) != 0 || cur.SandboxRef != "sbx" || st.Resume == nil {
		t.Errorf("%d stops, finalize = %v, %d give-ups, ref %q, record %+v; want a stop tried, an error, no give-up, the ref and the resume kept",
			stopper.stops.Load(), err, len(cs.giveUps), cur.SandboxRef, st)
	}
}

// A pass back that finds a sandbox kept for no resume already stopped writes its own record (no pass ran,
// uncounted): nothing was counted, so nothing is given up, and the end tail clears the ref. Catches: the kept
// sandbox's give-up taken for a run whose record holds no resume.
func TestFinalizeRunEffects_AnUnquiescedSandboxWithNoResumeIsNotGivenUp(t *testing.T) {
	s, cs, _, run, sb, _ := endGiveUpFixture(t, types.RunCompleted, types.RunLocalFolderStatus{Gen: 3, Settled: 3})
	cs.giveUpApplies = true
	sb.ended = true
	err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
	st := cs.folderStatus()
	cur, _ := cs.GetRun(context.Background(), run.ID)
	if err != nil || len(cs.giveUps) != 0 || st.Back == nil || st.Back.GivenUpAt != nil || !strings.Contains(st.Back.Error, "uncounted") || cur.SandboxRef != "" {
		t.Errorf("finalize = %v, %d give-ups, record %+v, ref %q; want nil, none, the uncounted record, the ref cleared",
			err, len(cs.giveUps), st.Back, cur.SandboxRef)
	}
}

// A kept sandbox its pass back cannot quiesce whose give-up fails keeps its resume and count, so the end's give-up
// after the stop tries again: once the store answers, the record is given up with the count and its row written,
// and the ref is cleared; while it keeps failing, the record still holds the resume, its generation settled, and
// the ref is kept with an error. Catches: a failed first give-up that falls through to the record saying the
// changes were not counted, which drops the resume, so the stop clears the ref with nothing given up.
func TestFinalizeRunEffects_AFailedKeptSandboxGiveUpKeepsItsCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heals bool
	}{
		{"the store answers again once the sandbox is stopped", true},
		{"the store keeps failing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, audit, run, sb, refAt := endGiveUpFixture(t, types.RunCompleted, keptForCopyBack())
			cs.giveUpApplies, cs.giveUpErr = true, errors.New("the database did not answer")
			sb.ended = true
			if tc.heals {
				sb.stopped = func() { cs.giveUpErr = nil }
			}
			err := s.finalizeRunEffects(context.Background(), run.ID, "sbx", "run.complete", nil, false, false)
			if sb.stopCount() != 1 || len(cs.giveUps) != 2 || slices.ContainsFunc(*refAt, func(r string) bool { return r != "sbx" }) {
				t.Fatalf("%d stops, %d give-ups, refs at give-up %v; want 1 and 2, each made while the ref was set", sb.stopCount(), len(cs.giveUps), *refAt)
			}
			for _, p := range cs.giveUps {
				if p.Cap != 0 || p.Back.GivenUpCount == nil || *p.Back.GivenUpCount != 3 || !slices.Equal(p.Back.GivenUpPaths, []string{"a.txt", "b.txt"}) {
					t.Errorf("give-up %+v; want a zero cap and 3 changes given up, 2 named", p)
				}
			}
			st := cs.folderStatus()
			cur, _ := cs.GetRun(context.Background(), run.ID)
			given, uncounted := endGiveUpRows(audit, run.ID, "changed files not brought back: 3"), endGiveUpRows(audit, run.ID, "uncounted")
			if tc.heals {
				if err != nil || st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil || cur.SandboxRef != "" || given != 1 || uncounted != 0 {
					t.Errorf("finalize = %v, record %+v, ref %q, %d counted and %d uncounted rows; want nil, given up, the ref cleared, 1 and 0",
						err, st, cur.SandboxRef, given, uncounted)
				}
				return
			}
			if err == nil || st.Resume == nil || st.Resume.Unsynced != 3 || st.Settled != st.Gen || (st.Back != nil && st.Back.GivenUpAt != nil) ||
				cur.SandboxRef != "sbx" || given != 0 || uncounted != 0 {
				t.Errorf("finalize = %v, record %+v, ref %q, %d counted and %d uncounted rows; want an error, the resume of 3 kept and settled, the ref kept, no rows",
					err, st, cur.SandboxRef, given, uncounted)
			}
		})
	}
}
