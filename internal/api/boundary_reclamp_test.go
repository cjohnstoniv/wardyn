// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// boundaryReclampStore is reclampStore with a Wardyn boundary.
type boundaryReclampStore struct {
	*reclampStore
	b types.Boundary
}

func (s *boundaryReclampStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *boundaryReclampStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// TestBoundaryCapsALiveRunsEndOnTheSweep: a live run whose profile allows 20 days and no end is held
// to the boundary's run-limit maximum on the next sweep, as a tightened profile is.
func TestBoundaryCapsALiveRunsEndOnTheSweep(t *testing.T) {
	limits := types.RunLimits{MaxEndAheadSec: 20 * 86400, AllowNoEnd: true, UserChangesLimits: true}
	f, st, profile := newReclampFixture(t, limits, 10*day, 0)
	st.setProfile(profile, limits)
	end := boundaryMaxEnd
	f.srv.cfg.Store = &boundaryReclampStore{reclampStore: st, b: types.Boundary{Mode: types.BoundaryHardLimits,
		Limits: types.BoundaryLimits{MaxEndAheadSec: &end}}}

	sweepLimits(t, f)

	run := st.current()
	if cut := f.now.Add(boundaryMaxEnd * time.Second); run.EndsAt == nil || run.EndsAt.After(cut) {
		t.Errorf("ends_at = %v, want at most %v", run.EndsAt, cut)
	}
	if run.RunLimits.MaxEndAheadSec != boundaryMaxEnd || run.RunLimits.AllowNoEnd {
		t.Errorf("captured limits = %+v, want the boundary's maximum and no No end", run.RunLimits)
	}
}
