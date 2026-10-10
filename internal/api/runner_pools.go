// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
)

// The runner pool routes (0.9). They are mounted behind their final tiers so the
// route-tier guards (authz_test.go's routeMatrix) know them before any storage
// exists, and every one answers 501 runner_pools_unavailable until the pool
// storage lands. The handler of each is the one place its lane writes:
//
//	catalogue reads, own-runner membership, personal defaults   member (the caller's own data only)
//	pool create/update/delete, executor membership, org default admin  (SUPER)
//	pool-use policy                                             security (admin or security_admin);
//	                                                            held for a second human under
//	                                                            WARDYN_GOVERNANCE_SECOND_HUMAN
//
// A person adds only their own claimed runner to a pool: no route here takes an
// owner, and none may claim, transfer or dispatch another person's runner.
func (s *Server) mountRunnerPoolRoutes(r, operatorOnly, securityOps chi.Router) {
	r.Get("/runner-pools", s.handleRunnerPoolsUnavailable)
	r.Get("/runner-pools/{id}", s.handleRunnerPoolsUnavailable)
	operatorOnly.Post("/runner-pools", s.handleRunnerPoolsUnavailable)
	operatorOnly.Put("/runner-pools/{id}", s.handleRunnerPoolsUnavailable)
	operatorOnly.Delete("/runner-pools/{id}", s.handleRunnerPoolsUnavailable)
	operatorOnly.Put("/runner-pools/{id}/executors/{executor}", s.handleRunnerPoolsUnavailable)
	operatorOnly.Delete("/runner-pools/{id}/executors/{executor}", s.handleRunnerPoolsUnavailable)
	r.Put("/me/runner-pools/{id}/runners/{runner}", s.handleRunnerPoolsUnavailable)
	r.Delete("/me/runner-pools/{id}/runners/{runner}", s.handleRunnerPoolsUnavailable)
	securityOps.Get("/runner-pools/{id}/use-policy", s.handleRunnerPoolsUnavailable)
	securityOps.Put("/runner-pools/{id}/use-policy", s.handleRunnerPoolsUnavailable)
	securityOps.Delete("/runner-pools/{id}/use-policy", s.handleRunnerPoolsUnavailable)
	r.Get("/runner-pool-defaults", s.handleRunnerPoolsUnavailable)
	operatorOnly.Put("/runner-pool-defaults", s.handleRunnerPoolsUnavailable)
	r.Get("/me/runner-pool-defaults", s.handleRunnerPoolsUnavailable)
	r.Put("/me/runner-pool-defaults", s.handleRunnerPoolsUnavailable)
	r.Delete("/me/runner-pool-defaults", s.handleRunnerPoolsUnavailable)
}

func (s *Server) handleRunnerPoolsUnavailable(w http.ResponseWriter, _ *http.Request) {
	writeErrorReason(w, runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
}
