// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unstartedEnd marks a context whose failures end a run that never started: failAndRevoke ends such a run with
// store.EndUnstartedRun, which claims the drive the run kept in the same statement, instead of the plain state
// write. It is carried by the pending abort and, for a macOS VM run, by the dispatch from the point its cleanup is
// registered. asked turns true directly before the runner is asked to create the sandbox: from then on a failure
// cannot say the Mac holds no disk, so it ends the run with the plain write and the drive is kept.
type unstartedEnd struct{ asked atomic.Bool }

type unstartedEndKey struct{}

func withUnstartedEnd(ctx context.Context, u *unstartedEnd) context.Context {
	return context.WithValue(ctx, unstartedEndKey{}, u)
}

func unstartedEndOf(ctx context.Context) *unstartedEnd {
	u, _ := ctx.Value(unstartedEndKey{}).(*unstartedEnd)
	return u
}

// unstartedEndStore is the store method that ends an unstarted run with the claim (store.PG).
type unstartedEndStore interface {
	EndUnstartedRun(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error)
}

// casRunStateEnding is casRunState for a failure that may end a run which never started: with the marker in ctx
// and no create asked yet, the end claims the run's kept drive in its own statement.
func (s *Server) casRunStateEnding(ctx context.Context, runID uuid.UUID, from, to types.RunState) (bool, error) {
	if u := unstartedEndOf(ctx); u != nil && !u.asked.Load() {
		if es, ok := s.cfg.Store.(unstartedEndStore); ok {
			applied, err := es.EndUnstartedRun(ctx, runID, from, to)
			s.runStateMoved(runID, from, to, applied)
			return applied, err
		}
	}
	return s.casRunState(ctx, runID, from, to)
}
