// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pool's use policy may name a unit (types.RunnerPoolSubjectUnit). Whether it admits is a fact about
// the run's unit, so it is read where the run's unit is known and never from the caller alone: the
// create, the list and the drives catalogue ask the caller's resolved unit; the queued start and the
// revive ask the unit the run froze. A unit that cannot be found, a read that fails and a run in no
// unit all leave callerSubjects.units empty, which matches no unit row: a use policy only narrows.
//
// The units are read only when a pool's policy names one, so a deployment that never writes such a row
// reads nothing more and resolves no extra ceiling.

// frozenPoolUnitKey carries, on a context, the unit a run froze (nil: it froze none), for the pool
// checks of a queued start.
type frozenPoolUnitKey struct{}

type frozenPoolUnit struct{ id *uuid.UUID }

// withFrozenPoolUnit makes the pool checks on ctx judge unit rows by the run's frozen unit instead of
// the caller's current one. A nil id is a run that froze none.
func withFrozenPoolUnit(ctx context.Context, id *uuid.UUID) context.Context {
	return context.WithValue(ctx, frozenPoolUnitKey{}, frozenPoolUnit{id: id})
}

// rowsNameUnit reports whether any pool in rows has a use policy with a unit row.
func rowsNameUnit(rows []store.RunnerPoolCatalogueRow) bool {
	for _, row := range rows {
		if row.Policy == nil {
			continue
		}
		for _, sub := range row.Policy.Subjects {
			if sub.SubjectType == types.RunnerPoolSubjectUnit {
				return true
			}
		}
	}
	return false
}

// unitLineage is id and every unit above it, as the strings a unit row is stored in, read through the
// resolver. A unit that no longer exists, a nil id, a read that failed or a chain the resolver refuses
// has none.
func (s *Server) unitLineage(ctx context.Context, id *uuid.UUID) []string {
	if id == nil || s.cfg.Store == nil {
		return nil
	}
	u, err := s.resolveUnitByID(ctx, *id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: read the unit chain for a pool use policy", slog.String("unit_id", id.String()), slog.Any("err", err))
		}
		return nil
	}
	out := make([]string, len(u.Chain))
	for i, l := range u.Chain {
		out[i] = l.ID.String()
	}
	return out
}

// poolUnits is the units a pool's unit rows are matched against on ctx: the frozen unit of a queued
// start when ctx carries one, else the unit the caller's ceiling resolves to.
func (s *Server) poolUnits(ctx context.Context) []string {
	if f, ok := ctx.Value(frozenPoolUnitKey{}).(frozenPoolUnit); ok {
		return s.unitLineage(ctx, f.id)
	}
	c, err := s.effectiveCeiling(ctx)
	if err != nil || c.Unit == nil {
		return nil
	}
	return s.unitLineage(ctx, &c.Unit.ID)
}

// poolSubjects is callerSubjects with the units, for the pools in rows: the one subject set every place
// that judges a pool for a caller reads.
func (s *Server) poolSubjects(ctx context.Context, rows []store.RunnerPoolCatalogueRow) (callerSubjects, error) {
	c, err := s.callerSubjects(ctx)
	if err != nil {
		return callerSubjects{}, err
	}
	if rowsNameUnit(rows) {
		c.units = s.poolUnits(ctx)
	}
	return c, nil
}
