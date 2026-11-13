// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failingRunStore's GetRun always fails, as a transient read fault on every pass does; every other store
// method panics (embedded nil interface).
type failingRunStore struct{ store.Store }

func (failingRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, errors.New("store unavailable")
}

// pageAfter pages ids in slice order, as the store's (stamp, id) order would; a cursor's ID is the last id of
// the page before.
func pageAfter(ids []uuid.UUID, after *store.SweepCursor, limit int) ([]uuid.UUID, *store.SweepCursor, error) {
	start := 0
	if after != nil {
		start = slices.Index(ids, after.ID) + 1
	}
	page := ids[start:min(start+limit, len(ids))]
	if len(page) < limit {
		return page, nil, nil
	}
	return page, &store.SweepCursor{ID: page[len(page)-1]}, nil
}

// pagedCastStore owes the casts of ids, terminal runs whose sweep fails on every pass (GetRun fails), and
// records the cursor each list was given.
type pagedCastStore struct {
	failingRunStore
	ids    []uuid.UUID
	afters []*store.SweepCursor
}

func (*pagedCastStore) SetRunAgentCastOwed(context.Context, uuid.UUID) error { return nil }
func (*pagedCastStore) DeclineRunAgentCast(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}
func (*pagedCastStore) ClearRunAgentCastOwed(context.Context, uuid.UUID, bool) error { return nil }
func (p *pagedCastStore) ListRunsAgentCastOwed(_ context.Context, _ time.Duration, after *store.SweepCursor, limit int) ([]uuid.UUID, *store.SweepCursor, error) {
	p.afters = append(p.afters, after)
	return pageAfter(p.ids, after, limit)
}

// A full page of runs whose sweep fails every time does not hold the sweep: the next pass starts after that
// page and reaches the run behind it, and the short page after it starts the pass after from the oldest.
func TestSweepAgentCasts_ACursorReachesRunsPastAPageThatAlwaysFails(t *testing.T) {
	ids := make([]uuid.UUID, agentCastSweepPage+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	st := &pagedCastStore{ids: ids}
	s := &Server{cfg: Config{Store: st, RecordingStore: fsRecordingStore(t)}}
	for i := 0; i < 3; i++ {
		if err := s.sweepAgentCasts(t.Context()); err == nil {
			t.Fatalf("sweep %d answered nil; every run's read fails", i)
		}
	}
	if len(st.afters) != 3 || st.afters[0] != nil || st.afters[1] == nil || st.afters[1].ID != ids[agentCastSweepPage-1] || st.afters[2] != nil {
		t.Fatalf("cursors given = %v; want none, then the last run of the first page, then none again", st.afters)
	}
}

// pagedPendingStore lists ids as stale pending rows and records the cursor each list was given; every other
// method panics (embedded nil interface).
type pagedPendingStore struct {
	store.RunOutputStore
	ids    []uuid.UUID
	afters []*store.SweepCursor
}

func (p *pagedPendingStore) ListStalePendingRunOutputs(_ context.Context, _ time.Duration, after *store.SweepCursor, limit int) ([]uuid.UUID, *store.SweepCursor, error) {
	p.afters = append(p.afters, after)
	return pageAfter(p.ids, after, limit)
}

// A full page of stale pending rows that stay pending (each run's read fails, so resolveStalePending leaves the
// row for the next sweep) does not hold the sweep: the next pass starts after that page.
func TestSweepStalePending_ACursorReachesRowsPastAPageThatAlwaysFails(t *testing.T) {
	ids := make([]uuid.UUID, staleOutputSweepPage+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	st := &pagedPendingStore{ids: ids}
	s := &Server{cfg: Config{Store: failingRunStore{}}}
	for i := 0; i < 3; i++ {
		if err := s.sweepStalePending(t.Context(), st); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(st.afters) != 3 || st.afters[0] != nil || st.afters[1] == nil || st.afters[1].ID != ids[staleOutputSweepPage-1] || st.afters[2] != nil {
		t.Fatalf("cursors given = %v; want none, then the last row of the first page, then none again", st.afters)
	}
}
