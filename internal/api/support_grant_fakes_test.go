// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeSupportGrants is run_support_grants in memory, with the behaviour store_support_grants.go gives it and a
// clock the test moves by hand (the database's now()). It reads each run's state and creator from runOf, so
// a test that flips the run's state flips what a grant's liveness reads. Lanes 2 to 4 and 6 build on it:
//
//	b := newSupportBackend()      // authzStore + fakeSupportGrants, one store.Store
//	b.putRun(run)                 // the run the grant is about
//	cfg := baseTestConfig(newHarness(t), b)
//
// and move time with b.tick, break a method with b.failOn("SupportGrantLive", err), and read the rows with
// b.row and b.rows.
type fakeSupportGrants struct {
	mu    sync.Mutex
	now   time.Time
	runOf func(uuid.UUID) (types.AgentRun, bool)
	list  []store.SupportGrant
	fail  map[string]error
	lives []supportLiveCall
}

// supportLiveCall is one SupportGrantLive call, so a test can pin who stamps last_seen_at.
type supportLiveCall struct {
	Run, Grant uuid.UUID
	Helper     string
	Stamp      bool
}

var supportFakeEpoch = time.Now().UTC().Truncate(time.Second)

func newFakeSupportGrants(runOf func(uuid.UUID) (types.AgentRun, bool)) *fakeSupportGrants {
	return &fakeSupportGrants{now: supportFakeEpoch, runOf: runOf, fail: map[string]error{}}
}

// tick moves the database's clock.
func (f *fakeSupportGrants) tick(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// failOn makes the named store method answer err (nil restores it).
func (f *fakeSupportGrants) failOn(method string, err error) {
	f.mu.Lock()
	f.fail[method] = err
	f.mu.Unlock()
}

// row is the grant's current row.
func (f *fakeSupportGrants) row(id uuid.UUID) (store.SupportGrant, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.list {
		if g.ID == id {
			return g, true
		}
	}
	return store.SupportGrant{}, false
}

// rows is every grant, in the order they were requested.
func (f *fakeSupportGrants) rows() []store.SupportGrant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list)
}

// liveCalls is every SupportGrantLive call so far.
func (f *fakeSupportGrants) liveCalls() []supportLiveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lives)
}

// put adds g as it stands, as another replica's write would have left it.
func (f *fakeSupportGrants) put(g store.SupportGrant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, g)
}

// edit changes one stored row in place, for a test that needs a state the routes cannot reach.
func (f *fakeSupportGrants) edit(id uuid.UUID, change func(*store.SupportGrant)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.list {
		if f.list[i].ID == id {
			change(&f.list[i])
		}
	}
}

func (f *fakeSupportGrants) end(i int, reason string, audited bool) {
	at := f.now
	f.list[i].EndedAt, f.list[i].EndReason, f.list[i].EndAudited = &at, reason, audited
}

// lapsedOrExpired is the statement RequestSupportGrant and the sweep share: a request unanswered 15 minutes, or
// an accepted grant at or past its expiry.
func (f *fakeSupportGrants) lapsedOrExpired(g store.SupportGrant) (string, bool) {
	if g.AcceptedAt == nil && !g.RequestedAt.After(f.now.Add(-15*time.Minute)) {
		return "lapsed", true
	}
	if g.ExpiresAt != nil && !g.ExpiresAt.After(f.now) {
		return "expired", true
	}
	return "", false
}

// runOver reports the run is not RUNNING or is lost; a run the fake cannot see counts as over.
func (f *fakeSupportGrants) runOver(run uuid.UUID) bool {
	r, ok := f.runOf(run)
	return !ok || r.State != types.RunRunning || r.LostAt != nil
}

func (f *fakeSupportGrants) RequestSupportGrant(_ context.Context, g store.SupportGrant) (store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["RequestSupportGrant"]; err != nil {
		return store.SupportGrant{}, err
	}
	for i, row := range f.list {
		if row.RunID != g.RunID || row.EndedAt != nil {
			continue
		}
		if reason, over := f.lapsedOrExpired(row); over {
			f.end(i, reason, false)
		}
	}
	for _, row := range f.list {
		if row.RunID == g.RunID && row.EndedAt == nil {
			return store.SupportGrant{}, store.ErrSupportGrantOpen
		}
	}
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	g.RequestedAt = f.now
	g.Doors, g.Minutes, g.AcceptedAt, g.ExpiresAt, g.LastSeenAt, g.EndedAt = nil, 0, nil, nil, nil, nil
	g.OwnerEmail, g.EndReason, g.EndAudited, g.AcceptRevSeq = "", "", false, 0
	f.list = append(f.list, g)
	return g, nil
}

func (f *fakeSupportGrants) AcceptSupportGrant(_ context.Context, run, grant uuid.UUID, owner, ownerEmail string, doors []string, minutes int, revSeq int64) (store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["AcceptSupportGrant"]; err != nil {
		return store.SupportGrant{}, err
	}
	if !slices.Contains([]int{15, 30, 60}, minutes) {
		return store.SupportGrant{}, fmt.Errorf("fake: minutes %d violates the table's CHECK", minutes)
	}
	r, found := f.runOf(run)
	for i, g := range f.list {
		if g.ID != grant || g.RunID != run || g.EndedAt != nil || g.AcceptedAt != nil || len(doors) == 0 ||
			!g.RequestedAt.After(f.now.Add(-15*time.Minute)) || !found || f.runOver(run) || r.CreatedBy != owner {
			continue
		}
		for _, d := range doors {
			if !slices.Contains(g.DoorsAsked, d) {
				return store.SupportGrant{}, store.ErrNotFound
			}
		}
		at, exp := f.now, f.now.Add(time.Duration(minutes)*time.Minute)
		f.list[i].AcceptedAt, f.list[i].ExpiresAt, f.list[i].Minutes = &at, &exp, minutes
		f.list[i].Doors, f.list[i].AcceptRevSeq, f.list[i].OwnerEmail = slices.Clone(doors), revSeq, ownerEmail
		return f.list[i], nil
	}
	return store.SupportGrant{}, store.ErrNotFound
}

func (f *fakeSupportGrants) DeclineSupportGrant(_ context.Context, run, grant uuid.UUID, owner string) (store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["DeclineSupportGrant"]; err != nil {
		return store.SupportGrant{}, err
	}
	r, found := f.runOf(run)
	for i, g := range f.list {
		if g.ID == grant && g.RunID == run && g.EndedAt == nil && g.AcceptedAt == nil && found && r.CreatedBy == owner {
			f.end(i, "declined", true)
			return f.list[i], nil
		}
	}
	return store.SupportGrant{}, store.ErrNotFound
}

func (f *fakeSupportGrants) EndSupportGrant(_ context.Context, run, grant uuid.UUID, reason string, audited bool) (store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["EndSupportGrant"]; err != nil {
		return store.SupportGrant{}, err
	}
	for i, g := range f.list {
		if g.ID == grant && g.RunID == run && g.EndedAt == nil {
			f.end(i, reason, audited)
			return f.list[i], nil
		}
	}
	return store.SupportGrant{}, store.ErrNotFound
}

func (f *fakeSupportGrants) SupportGrantLive(_ context.Context, run uuid.UUID, helper string, grant uuid.UUID, stamp bool) (store.SupportGrant, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lives = append(f.lives, supportLiveCall{Run: run, Helper: helper, Grant: grant, Stamp: stamp})
	if err := f.fail["SupportGrantLive"]; err != nil {
		return store.SupportGrant{}, false, false, err
	}
	for i, g := range f.list {
		if g.RunID != run || g.Helper != helper || g.EndedAt != nil || (grant != uuid.Nil && g.ID != grant) {
			continue
		}
		over := f.runOver(run)
		live := g.AcceptedAt != nil && g.ExpiresAt != nil && f.now.Before(*g.ExpiresAt) && !over
		if g.AcceptedAt != nil && !live {
			reason := "expired"
			if over {
				reason = "run_ended"
			}
			f.end(i, reason, false)
			return f.list[i], true, false, nil
		}
		if stamp && live && (g.LastSeenAt == nil || g.LastSeenAt.Before(f.now.Add(-30*time.Second))) {
			at := f.now
			f.list[i].LastSeenAt = &at
		}
		return f.list[i], true, live, nil
	}
	return store.SupportGrant{}, false, false, nil
}

func (f *fakeSupportGrants) SupportGrantStatus(_ context.Context, run uuid.UUID) (*store.SupportGrant, *store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["SupportGrantStatus"]; err != nil {
		return nil, nil, err
	}
	var open, ended *store.SupportGrant
	for i, g := range f.list {
		switch {
		case g.RunID != run:
		case g.EndedAt == nil:
			c := f.list[i]
			open = &c
		case ended == nil || g.EndedAt.After(*ended.EndedAt) || (g.EndedAt.Equal(*ended.EndedAt) && g.RequestedAt.After(ended.RequestedAt)):
			c := f.list[i]
			ended = &c
		}
	}
	return open, ended, nil
}

func (f *fakeSupportGrants) SweepSupportGrants(_ context.Context, off bool) ([]store.SupportGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["SweepSupportGrants"]; err != nil {
		return nil, err
	}
	for i, g := range f.list {
		if g.EndedAt == nil && f.runOver(g.RunID) {
			f.end(i, "run_ended", false)
		}
	}
	for i, g := range f.list {
		if reason, over := f.lapsedOrExpired(g); g.EndedAt == nil && over {
			f.end(i, reason, false)
		}
	}
	if off {
		for i, g := range f.list {
			if g.EndedAt == nil {
				f.end(i, "support_off", false)
			}
		}
	}
	var claimed []store.SupportGrant
	for i, g := range f.list {
		if g.EndedAt != nil && !g.EndAudited && len(claimed) < 100 {
			f.list[i].EndAudited = true
			claimed = append(claimed, f.list[i])
		}
	}
	return claimed, nil
}

func (f *fakeSupportGrants) LiveSupportGrants(context.Context) ([]store.SupportGrantRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["LiveSupportGrants"]; err != nil {
		return nil, err
	}
	var refs []store.SupportGrantRef
	for _, g := range f.list {
		if g.EndedAt == nil && g.AcceptedAt != nil && g.ExpiresAt != nil && g.ExpiresAt.After(f.now) {
			refs = append(refs, store.SupportGrantRef{RunID: g.RunID, ID: g.ID})
		}
	}
	return refs, nil
}

func (f *fakeSupportGrants) SupportHelperSince(_ context.Context, run uuid.UUID, principal string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["SupportHelperSince"]; err != nil {
		return false, err
	}
	for _, g := range f.list {
		if g.RunID == run && g.Helper == principal && g.AcceptedAt != nil && (at.IsZero() || !g.RequestedAt.After(at)) {
			return true, nil
		}
	}
	return false, nil
}

var _ store.SupportGrantStore = (*fakeSupportGrants)(nil)

// supportBackend is the api tests' run store (authzStore) with the grant record on top, one store.Store.
type supportBackend struct {
	*authzStore
	*fakeSupportGrants
}

func newSupportBackend() *supportBackend {
	ast := newAuthzStore()
	return &supportBackend{authzStore: ast, fakeSupportGrants: newFakeSupportGrants(func(id uuid.UUID) (types.AgentRun, bool) {
		ast.mu.Lock()
		defer ast.mu.Unlock()
		r, ok := ast.runs[id]
		return r, ok
	})}
}

// putRun stores run, replacing the run of that id: a test flips a run's state or loses it by putting it again.
func (b *supportBackend) putRun(run types.AgentRun) {
	b.authzStore.mu.Lock()
	b.authzStore.runs[run.ID] = run
	b.authzStore.mu.Unlock()
}
