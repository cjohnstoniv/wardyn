// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountWallsRoutes mounts the Walls view's four reads on the authenticated group r. The topology and its live
// stream are the unit-admin tier's (requireUnitScope): a security caller passes, a unit admin reads their own
// units. A run's walls are its owner's and the security tier's (ownsRunOrAdmin, in the handler), and a run's live
// stream also admits a unit admin of the run's unit (wallsRunReadable).
func (s *Server) mountWallsRoutes(r chi.Router) {
	unit := r.With(s.requireUnitScope)
	unit.Get("/walls/topology", s.handleWallsTopology)
	unit.Get("/walls/stream", s.handleWallsStreamStub)
	r.Get("/runs/{id}/walls", s.handleRunWalls)
	r.Get("/runs/{id}/walls/stream", s.handleRunWallsStreamStub)
}

// The fallback: mount the stub in place of a handler whose body is held back; the gate, and every access row,
// stay. A unit-tier stub sits behind requireUnitScope, which has already gated it. A run stub runs its route's
// run gate first, so a caller who may not read the run gets the missing run's 404 before the 501.

func (s *Server) handleWallsTopologyStub(w http.ResponseWriter, _ *http.Request) {
	writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this Walls view route is not available on this server yet")
}

func (s *Server) handleWallsStreamStub(w http.ResponseWriter, _ *http.Request) {
	writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this Walls view route is not available on this server yet")
}

func (s *Server) handleRunWallsStub(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin); !ok {
		return
	}
	writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this Walls view route is not available on this server yet")
}

func (s *Server) handleRunWallsStreamStub(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorizedBy(w, r, id, s.wallsRunReadable); !ok {
		return
	}
	writeErrorReason(w, http.StatusNotImplemented, reasonWallsViewUnavailable, "this Walls view route is not available on this server yet")
}

// wallsUnitRunScope: the caller is a unit admin and run.UnitID is non-nil and inside their units
// (unitAuditScopeFor's set). False for every other caller, and for a run with no unit. The scope is read on
// every request, never kept, so a revoked grant stops serving on the next one; a scope unitAuditScopeFor cannot
// give (nil) is the empty set.
func (s *Server) wallsUnitRunScope(r *http.Request, run types.AgentRun) bool {
	if !s.isUnitAdminCaller(r.Context()) || run.UnitID == nil {
		return false
	}
	scope := s.unitAuditScopeFor(r, true, run)
	return scope != nil && slices.Contains(scope.units, *run.UnitID)
}

// wallsRunReadable is the run gate of a run's live stream: the run's owner, the security tier, or a unit admin
// of the run's unit.
func (s *Server) wallsRunReadable(r *http.Request, run types.AgentRun) bool {
	return s.ownsRunOrAdmin(r, run) || s.wallsUnitRunScope(r, run)
}

// refuseWallsRate charges l for the caller and writes the 429 when it is spent; perMin feeds Retry-After. The
// admin token and a device are shared actors and are not counted (rateLimitedActor).
func (s *Server) refuseWallsRate(w http.ResponseWriter, r *http.Request, l *principalLimiter, perMin int) bool {
	if who, limited := rateLimitedActor(r); !limited || l.allow(who, s.cfg.Now()) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(60/float64(perMin))))))
	writeErrorReason(w, http.StatusTooManyRequests, reasonWallsRateLimited, "too many Walls view reads; slow down")
	return true
}

// wallsState is the Walls view's process state on one replica: its three limiters, set once by wallsLimiters,
// and the topology snapshots continuation pages read.
type wallsState struct {
	wallsOnce                sync.Once
	wallsTopologyLimiter     principalLimiter   // first topology pages: 6 a minute, burst 3, per caller on this replica
	wallsRunLimiter          principalLimiter   // a run's walls: 30 a minute, burst 10, per caller
	wallsContinuationLimiter principalLimiter   // topology continuation pages: 60 a minute, burst 10, per caller
	wallsSnapshots           wallsSnapshotCache // walls_topology.go
	wallsCaps                wallsCaps          // a zero field takes wallsDefaultCaps' value; tests set smaller ones
}

// wallsCaps are the read bounds: the runs in one snapshot, the rows one decision read asks for, the reads one
// snapshot may make, the rows the count read may count, and the reads a run's walls may make.
type wallsCaps struct{ runs, readLimit, reads, countRows, runReads int }

// wallsDefaultCaps holds the bounds a zero field of Server.wallsCaps takes, and is their only literal outside tests.
func wallsDefaultCaps() wallsCaps {
	return wallsCaps{runs: 5000, readLimit: 5000, reads: 50, countRows: 250000, runReads: 2}
}

// wallsLimiters is the three Walls view limiters, with their rate, burst and table size set once.
func (s *Server) wallsLimiters() (topology, continuation, run *principalLimiter) {
	s.wallsOnce.Do(func() {
		s.wallsTopologyLimiter.rate, s.wallsTopologyLimiter.burst = 6.0/60, 3
		s.wallsContinuationLimiter.rate, s.wallsContinuationLimiter.burst = 60.0/60, 10
		s.wallsRunLimiter.rate, s.wallsRunLimiter.burst = 30.0/60, 10
		s.wallsTopologyLimiter.max = preflightLimiterMaxPeople
		s.wallsContinuationLimiter.max = preflightLimiterMaxPeople
		s.wallsRunLimiter.max = preflightLimiterMaxPeople
	})
	return &s.wallsTopologyLimiter, &s.wallsContinuationLimiter, &s.wallsRunLimiter
}
