// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// runCapacityReader is the OPTIONAL store capability the fleet capacity view needs, kept off
// store.Store for the reason runStatusDetailSetter states.
type runCapacityReader interface {
	RunCapacity(ctx context.Context, o store.RunCapacityOpts) (store.RunCapacity, error)
}

// capacityBasisConfigured names what every figure in the response is: the reservation recorded
// at dispatch, not a measurement.
const capacityBasisConfigured = "configured_reservations"

// capacityBlockerReason is the reason token of a status_detail that is a capacity blocker, or "".
func capacityBlockerReason(detail string) string {
	if reason := statusDetailReason(detail); runner.CapacityBlockerReasons[reason] {
		return reason
	}
	return ""
}

// deploymentExecutor reports whether executor names one of this deployment's own substrates: each
// local one of an orchestrator, or the lone driver. It is not equality to the aggregate driver's
// Name(), which reads "orchestrator" when several run.
func (s *Server) deploymentExecutor(executor string) bool {
	if s.cfg.Runner == nil {
		return false
	}
	names := []string{s.cfg.Runner.Name()}
	if ex, ok := s.cfg.Runner.(interface{ Executors() []string }); ok {
		names = ex.Executors()
	}
	return slices.Contains(names, executor)
}

// handleRunCapacity is GET /api/v1/admin/runs/capacity: the fleet's configured reservations,
// computed on request from one query over the non-terminal rows. It never execs into a sandbox
// or calls the runner. A failed teardown of a terminal run is not counted (OPERATIONS.md).
func (s *Server) handleRunCapacity(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.cfg.Store.(runCapacityReader)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunCapacityStoreUnavailable, "this store cannot report fleet capacity")
		return
	}
	now := time.Now().UTC()
	c, err := reader.RunCapacity(r.Context(), store.RunCapacityOpts{
		Now: now, CurrentKind: s.cfg.RunnerTarget, UnschedulableReason: capacityBlockerReason,
		LocalExecutor: s.deploymentExecutor,
	})
	if err != nil {
		writeServerError(w, r, "run capacity", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		GeneratedAt time.Time `json:"generated_at"`
		Basis       string    `json:"basis"`
		store.RunCapacity
	}{now, capacityBasisConfigured, c})
}
