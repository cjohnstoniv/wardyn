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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type reclaimFenceKey struct {
	backend types.DriveBackend
	object  string
}

type reclaimFenceRow struct {
	started time.Time
	issues  int
}

// reclaimFenceFake mirrors drive_reclaims and drive_object_holds statement by statement (internal/store/
// drive_reclaims.go) over maps. now is the database clock the one-minute takeover reads; each *Err fails its method
// (clearErr both clear methods); held is the set DriveObjectHeld answers from; steps logs the methods in call order.
type reclaimFenceFake struct {
	mu    sync.Mutex
	now   time.Time
	rows  map[reclaimFenceKey]reclaimFenceRow
	held  map[reclaimFenceKey]bool
	holds map[reclaimFenceKey][]uuid.UUID
	steps []string

	beginErr, issueErr, heldErr, holdErr, fencedErr, clearErr, finishErr error
}

func newReclaimFenceFake() *reclaimFenceFake {
	return &reclaimFenceFake{
		now:   time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
		rows:  map[reclaimFenceKey]reclaimFenceRow{},
		held:  map[reclaimFenceKey]bool{},
		holds: map[reclaimFenceKey][]uuid.UUID{},
	}
}

// reclaimFencedStore is a drive CRUD store that also carries the fence.
type reclaimFencedStore struct {
	*driveCRUDStore
	*reclaimFenceFake
}

var _ store.DriveReclaimFenceStore = (*reclaimFenceFake)(nil)

func (f *reclaimFenceFake) step(name string) {
	f.steps = append(f.steps, name)
}

func (f *reclaimFenceFake) BeginDriveReclaim(_ context.Context, b types.DriveBackend, object string, _ uuid.UUID) (time.Time, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step("begin")
	if f.beginErr != nil {
		return time.Time{}, 0, f.beginErr
	}
	k := reclaimFenceKey{b, object}
	row, ok := f.rows[k]
	if ok && !row.started.Before(f.now.Add(-time.Minute)) {
		return time.Time{}, 0, store.ErrDriveReclaimBusy
	}
	row.started = f.now
	f.rows[k] = row
	return row.started, row.issues, nil
}

func (f *reclaimFenceFake) IssueDriveReclaim(_ context.Context, b types.DriveBackend, object string, started time.Time, n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step("issue")
	if f.issueErr != nil {
		return 0, f.issueErr
	}
	k := reclaimFenceKey{b, object}
	row, ok := f.rows[k]
	if !ok || !row.started.Equal(started) || row.issues != n {
		return 0, store.ErrNotFound
	}
	row.issues += 1 + row.issues%2
	f.rows[k] = row
	return row.issues, nil
}

// settle deletes the row when it carries (started, n) and ok holds, else ErrNotFound.
func (f *reclaimFenceFake) settle(name string, failWith error, b types.DriveBackend, object string, started time.Time, n int, ok bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step(name)
	if failWith != nil {
		return failWith
	}
	k := reclaimFenceKey{b, object}
	row, found := f.rows[k]
	if !found || !row.started.Equal(started) || row.issues != n || !ok {
		return store.ErrNotFound
	}
	delete(f.rows, k)
	return nil
}

func (f *reclaimFenceFake) ClearDriveReclaim(_ context.Context, b types.DriveBackend, object string, started time.Time, n int) error {
	return f.settle("clear", f.clearErr, b, object, started, n, n%2 == 0)
}

func (f *reclaimFenceFake) ClearDriveReclaimIssued(_ context.Context, b types.DriveBackend, object string, started time.Time, claimN, issued int) error {
	return f.settle("clearIssued", f.clearErr, b, object, started, issued, claimN%2 == 0 && issued == claimN+1)
}

func (f *reclaimFenceFake) FinishDriveReclaim(_ context.Context, b types.DriveBackend, object string, started time.Time, n int) error {
	return f.settle("finish", f.finishErr, b, object, started, n, true)
}

func (f *reclaimFenceFake) DriveReclaimFenced(_ context.Context, b types.DriveBackend, object string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step("fenced")
	if f.fencedErr != nil {
		return false, f.fencedErr
	}
	_, ok := f.rows[reclaimFenceKey{b, object}]
	return ok, nil
}

func (f *reclaimFenceFake) HoldDriveObject(_ context.Context, b types.DriveBackend, object string, run uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step("hold")
	if f.holdErr != nil {
		return f.holdErr
	}
	k := reclaimFenceKey{b, object}
	if !slices.Contains(f.holds[k], run) {
		f.holds[k] = append(f.holds[k], run)
	}
	return nil
}

func (f *reclaimFenceFake) DriveObjectHeld(_ context.Context, b types.DriveBackend, object string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step("held")
	if f.heldErr != nil {
		return false, f.heldErr
	}
	return f.held[reclaimFenceKey{b, object}], nil
}

// state is the object's fence as a test reads it, outside the step log.
func (f *reclaimFenceFake) state(b types.DriveBackend, object string) (fenced bool, issues int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[reclaimFenceKey{b, object}]
	return ok, row.issues
}

// fence leaves a claim with one call outstanding on the object, as a reclaim that timed out does.
func (f *reclaimFenceFake) fence(b types.DriveBackend, object string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[reclaimFenceKey{b, object}] = reclaimFenceRow{started: f.now, issues: 1}
}

func (f *reclaimFenceFake) stepLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.steps)
}

// fenceOf is the fake reclaimServer wired in.
func fenceOf(t *testing.T, srv *Server) *reclaimFenceFake {
	t.Helper()
	fs, ok := srv.cfg.Store.(*reclaimFencedStore)
	if !ok {
		t.Fatalf("store %T carries no reclaim fence", srv.cfg.Store)
	}
	return fs.reclaimFenceFake
}

// reclaimObjectOf is the object a reclaim of reclaimSubject names on d.
func reclaimObjectOf(t *testing.T, d types.UserDrive) (home, object string) {
	t.Helper()
	home, err := types.DriveHomeName(d, reclaimSubject, "")
	if err != nil {
		t.Fatalf("derive home: %v", err)
	}
	return home, types.DriveObjectName(d, home)
}

// fenceWatchRunner is a reclaimRunner that records the fence at the moment of the call, and answers Capabilities so
// the launch door (driveBindFailureHere) can ask it.
type fenceWatchRunner struct {
	*reclaimRunner
	fence        *reclaimFenceFake
	fencedAtCall bool
	issuesAtCall int
	stepsAtCall  []string
}

func (r *fenceWatchRunner) ReclaimDrive(ctx context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	if r.fence != nil {
		r.fencedAtCall, r.issuesAtCall = r.fence.state(m.Backend, m.ObjectName)
		r.stepsAtCall = r.fence.stepLog()
	}
	return r.reclaimRunner.ReclaimDrive(ctx, m)
}

func (r *fenceWatchRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "docker", UserDrives: true}, nil
}

const reclaimBody = `{"subject_type":"user","subject":"` + reclaimSubject + `"}`

// TestReclaimFence_ClaimsBeforeTheCallAndSettlesOnConfirmedGone: the claim and its issue are recorded before the one
// substrate call, and a confirmed gone lifts the fence; a finish that cannot be written leaves it and says so.
func TestReclaimFence_ClaimsBeforeTheCallAndSettlesOnConfirmedGone(t *testing.T) {
	for _, outcome := range []runner.DriveReclaimOutcome{runner.DriveReclaimDeleted, runner.DriveReclaimAlreadyAbsent} {
		t.Run(string(outcome), func(t *testing.T) {
			d := reclaimDrive(nil)
			rn := &fenceWatchRunner{reclaimRunner: &reclaimRunner{outcome: outcome}}
			srv, _, _ := reclaimServer(t, d, nil, rn)
			f := fenceOf(t, srv)
			rn.fence = f
			_, object := reclaimObjectOf(t, d)

			if w := reclaimCall(t, srv, d.ID, reclaimBody); w.Code != http.StatusOK {
				t.Fatalf("reclaim = %d, want 200: %s", w.Code, w.Body.String())
			}
			if want := []string{"begin", "held", "issue"}; !slices.Equal(rn.stepsAtCall, want) {
				t.Errorf("steps before the call = %v, want %v", rn.stepsAtCall, want)
			}
			if !rn.fencedAtCall || rn.issuesAtCall != 1 {
				t.Errorf("at the call: fenced %v issues %d, want true 1", rn.fencedAtCall, rn.issuesAtCall)
			}
			if steps := f.stepLog(); steps[len(steps)-1] != "finish" {
				t.Errorf("steps = %v, want finish last", steps)
			}
			if fenced, _ := f.state(d.Backend, object); fenced {
				t.Error("a confirmed gone left the object fenced")
			}
		})
	}
	t.Run("the finish cannot be written", func(t *testing.T) {
		d := reclaimDrive(nil)
		rn := &fenceWatchRunner{reclaimRunner: &reclaimRunner{outcome: runner.DriveReclaimDeleted}}
		srv, _, audit := reclaimServer(t, d, nil, rn)
		f := fenceOf(t, srv)
		f.finishErr = errors.New("store: finish drive reclaim: conn reset")
		_, object := reclaimObjectOf(t, d)

		w := reclaimCall(t, srv, d.ID, reclaimBody)
		if reason, _ := errReason(t, w.Body.Bytes()); w.Code != http.StatusInternalServerError || reason != reasonUserDriveReclaimFailed {
			t.Fatalf("reclaim = %d %q, want 500 %s: %s", w.Code, reason, reasonUserDriveReclaimFailed, w.Body.String())
		}
		if len(rn.calls) != 1 {
			t.Errorf("substrate calls = %d, want 1", len(rn.calls))
		}
		if _, data := reclaimRow(t, audit); data["outcome"] != string(runner.DriveReclaimDeleted) {
			t.Errorf("audit row = %v, want outcome deleted (the only record of the destroy)", data)
		}
		if fenced, _ := f.state(d.Backend, object); !fenced {
			t.Error("a finish that failed lifted the fence")
		}
	})
}

// TestReclaimFence_RefusalsSettleTheClaim: each refusal before or at the call settles the claim exactly as far as
// what it proved, and a settle that fails leaves the object fenced and answers so.
func TestReclaimFence_RefusalsSettleTheClaim(t *testing.T) {
	type fenceWant int
	const (
		lifted fenceWant = iota
		kept
		untouchedClaim // the claim Begin took, issues 0: nothing settled it
		noFence
	)
	for _, tc := range []struct {
		name    string
		set     func(st *driveCRUDStore, f *reclaimFenceFake, object string)
		callErr error
		plain   bool
		code    int
		reason  string
		body    string
		calls   int
		outcome string
		fence   fenceWant
	}{
		{name: "this person starting", set: func(st *driveCRUDStore, _ *reclaimFenceFake, _ string) {
			st.starting = map[string]bool{reclaimSubject: true}
		}, code: http.StatusConflict, outcome: driveReclaimOutcomeRefused, fence: lifted},
		{name: "the clear fails after this person starting", set: func(st *driveCRUDStore, f *reclaimFenceFake, _ string) {
			st.starting = map[string]bool{reclaimSubject: true}
			f.clearErr = errors.New("store: clear drive reclaim: conn reset")
		}, code: http.StatusInternalServerError, reason: reasonUserDriveReclaimFailed, outcome: driveReclaimOutcomeRefused, fence: kept},
		{name: "held", set: func(_ *driveCRUDStore, f *reclaimFenceFake, object string) {
			f.held[reclaimFenceKey{types.DriveBackendDockerVolume, object}] = true
		}, code: http.StatusConflict, body: "a run that has not ended holds this object", outcome: driveReclaimOutcomeRefused, fence: lifted},
		{name: "the held read fails", set: func(_ *driveCRUDStore, f *reclaimFenceFake, _ string) {
			f.heldErr = errors.New("store: read held drive reclaim: conn reset")
		}, code: http.StatusInternalServerError, outcome: driveReclaimOutcomeFailed, fence: lifted},
		{name: "in use", callErr: runner.ErrDriveInUse, code: http.StatusConflict, calls: 1, outcome: driveReclaimOutcomeRefused, fence: lifted},
		{name: "untouched not reclaimable", callErr: runner.Untouched(runner.ErrDriveNotReclaimable), code: http.StatusConflict, calls: 1,
			outcome: driveReclaimOutcomeRefused, fence: lifted},
		{name: "bare not reclaimable", callErr: runner.ErrDriveNotReclaimable, code: http.StatusConflict, calls: 1,
			outcome: driveReclaimOutcomeRefused, fence: kept},
		{name: "the issue finds the claim taken over", set: func(_ *driveCRUDStore, f *reclaimFenceFake, _ string) {
			f.issueErr = store.ErrNotFound
		}, code: http.StatusConflict, outcome: driveReclaimOutcomeRefused, fence: untouchedClaim},
		{name: "the claim cannot be written", set: func(_ *driveCRUDStore, f *reclaimFenceFake, _ string) {
			f.beginErr = errors.New("store: begin drive reclaim: conn reset")
		}, code: http.StatusInternalServerError, outcome: driveReclaimOutcomeFailed, fence: lifted},
		{name: "a store without the fence", plain: true, code: http.StatusInternalServerError, reason: reasonUserDriveReclaimFailed,
			outcome: driveReclaimOutcomeFailed, fence: noFence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := reclaimDrive(nil)
			rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted, err: tc.callErr}
			srv, st, audit := reclaimServer(t, d, nil, rn)
			f := fenceOf(t, srv)
			_, object := reclaimObjectOf(t, d)
			if tc.set != nil {
				tc.set(st, f, object)
			}
			if tc.plain {
				srv.cfg.Store = st
			}

			w := reclaimCall(t, srv, d.ID, reclaimBody)
			if w.Code != tc.code {
				t.Fatalf("reclaim = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if reason, msg := errReason(t, w.Body.Bytes()); (tc.reason != "" && reason != tc.reason) || !strings.Contains(msg, tc.body) {
				t.Errorf("body = %q %q, want reason %q naming %q", reason, msg, tc.reason, tc.body)
			}
			if len(rn.calls) != tc.calls {
				t.Errorf("substrate calls = %d, want %d", len(rn.calls), tc.calls)
			}
			if _, data := reclaimRow(t, audit); data["outcome"] != tc.outcome {
				t.Errorf("audit row = %v, want outcome %q", data, tc.outcome)
			}
			fenced, issues := f.state(d.Backend, object)
			switch tc.fence {
			case lifted:
				if fenced {
					t.Error("the object is still fenced")
				}
			case kept:
				if !fenced {
					t.Error("the object is no longer fenced")
				}
			case untouchedClaim:
				if !fenced || issues != 0 {
					t.Errorf("fence = %v issues %d, want the claim as Begin left it", fenced, issues)
				}
			case noFence:
				if len(f.stepLog()) != 0 {
					t.Errorf("a store without the fence reached the fake: %v", f.stepLog())
				}
			}
		})
	}
}

// TestReclaimFence_AnotherPersonsLiveRunHoldsTheObject: a run that has not ended holds the object whoever started it.
func TestReclaimFence_AnotherPersonsLiveRunHoldsTheObject(t *testing.T) {
	for _, held := range []bool{true, false} {
		d := reclaimDrive(nil)
		rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
		srv, st, _ := reclaimServer(t, d, nil, rn)
		f := fenceOf(t, srv)
		_, object := reclaimObjectOf(t, d)
		st.starting = map[string]bool{"sub-drive-carol": true}
		if held {
			f.held[reclaimFenceKey{d.Backend, object}] = true
		}
		w := reclaimCall(t, srv, d.ID, reclaimBody)
		switch {
		case held && (w.Code != http.StatusConflict || len(rn.calls) != 0):
			t.Errorf("another person's live run holds it: reclaim = %d with %d calls, want 409 and none: %s", w.Code, len(rn.calls), w.Body.String())
		case !held && (w.Code != http.StatusOK || len(rn.calls) != 1):
			t.Errorf("no hold: reclaim = %d with %d calls, want 200 and 1: %s", w.Code, len(rn.calls), w.Body.String())
		}
	}
}
