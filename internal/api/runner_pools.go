// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The runner pool routes (0.9). A pool is an organisation-defined label for a choice and never
// authority: choosing one grants no runner, credential, drive or confinement class.
//
//	catalogue reads, own-runner membership, personal defaults   member (the caller's own data only)
//	pool create/update/delete, executor membership, org default admin  (SUPER)
//	pool-use policy                                             security (admin or security_admin);
//	                                                            held for a second human under
//	                                                            WARDYN_GOVERNANCE_SECOND_HUMAN
//
// A person adds only their own claimed runner to a pool: no route here takes an owner, and none may
// claim, transfer or dispatch another person's runner. A store without pool storage answers every
// route 501 runner_pools_unavailable.
func (s *Server) mountRunnerPoolRoutes(r, operatorOnly, securityOps chi.Router) {
	r.Get("/runner-pools", s.handleListRunnerPools)
	r.Get("/runner-pools/{id}", s.handleGetRunnerPool)
	operatorOnly.Post("/runner-pools", s.handleCreateRunnerPool)
	operatorOnly.Put("/runner-pools/{id}", s.handleUpdateRunnerPool)
	operatorOnly.Delete("/runner-pools/{id}", s.handleDeleteRunnerPool)
	operatorOnly.Put("/runner-pools/{id}/executors/{executor}", s.handleAddRunnerPoolExecutor)
	operatorOnly.Delete("/runner-pools/{id}/executors/{executor}", s.handleRemoveRunnerPoolExecutor)
	r.Put("/me/runner-pools/{id}/runners/{runner}", s.handleAddMyRunnerToPool)
	r.Delete("/me/runner-pools/{id}/runners/{runner}", s.handleRemoveMyRunnerFromPool)
	securityOps.Get("/runner-pools/{id}/use-policy", s.handleGetRunnerPoolUsePolicy)
	securityOps.Put("/runner-pools/{id}/use-policy", s.handlePutRunnerPoolUsePolicy)
	securityOps.Delete("/runner-pools/{id}/use-policy", s.handleDeleteRunnerPoolUsePolicy)
	r.Get("/runner-pool-defaults", s.handleGetOrgRunnerPoolDefaults)
	operatorOnly.Put("/runner-pool-defaults", s.handlePutOrgRunnerPoolDefaults)
	r.Get("/me/runner-pool-defaults", s.handleGetMyRunnerPoolDefaults)
	r.Put("/me/runner-pool-defaults", s.handlePutMyRunnerPoolDefaults)
	r.Delete("/me/runner-pool-defaults", s.handleDeleteMyRunnerPoolDefaults)
}

// The audit actions of the pool catalogue. Names carry no secret value.
const (
	auditPoolCreate         = "runner_pool.create"
	auditPoolUpdate         = "runner_pool.update"
	auditPoolDelete         = "runner_pool.delete"
	auditPoolExecutorAdd    = "runner_pool.executor.attach"
	auditPoolExecutorRemove = "runner_pool.executor.detach"
	auditPoolRunnerAdd      = "runner_pool.runner.attach"
	auditPoolRunnerRemove   = "runner_pool.runner.detach"
	auditPoolUsePolicySet   = "runner_pool.use_policy.set"
	auditPoolUsePolicyClear = "runner_pool.use_policy.delete"
	auditPoolDefaultSet     = "runner_pool.default.set"
	auditPersonDefaultSet   = "person.runner_pool_default.set"
	auditPersonDefaultClear = "person.runner_pool_default.delete"
)

func (s *Server) runnerPoolStore(w http.ResponseWriter) (store.RunnerPoolStore, bool) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		writeErrorReason(w, runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
	}
	return ps, ok
}

// writePoolRefusal answers a pool refusal with its status and the server's sentence for the reason.
func writePoolRefusal(w http.ResponseWriter, ref runnerpool.Refusal) {
	writeErrorReason(w, ref.Reason.Status(), string(ref.Reason), ref.Message())
}

// writePoolNotFound is the one answer for a pool that does not exist, was deleted, or that the caller
// may not use: indistinguishable, and carrying no name, member or count.
func writePoolNotFound(w http.ResponseWriter) {
	writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonNotFound})
}

func writePoolInvalid(w http.ResponseWriter, msg string) {
	writeErrorReason(w, runnerpool.ReasonInvalid.Status(), string(runnerpool.ReasonInvalid), msg)
}

// matchesPoolSubject reports whether a use-policy subject names the caller. A subject is stored in the
// canonical form the caller's identities are carried in, so equality is the whole test; a group
// snapshot that is missing or truncated simply matches nothing, which fails closed.
func (c callerSubjects) matchesPoolSubject(sub types.RunnerPoolSubject) bool {
	switch sub.SubjectType {
	case types.CapabilitySubjectUser:
		return slices.Contains(c.users, sub.Subject)
	case types.CapabilitySubjectGroup:
		return slices.Contains(c.groups, sub.Subject)
	case types.CapabilitySubjectUserType:
		return c.userType != "" && c.userType == sub.Subject
	}
	return false
}

// permits reports whether the caller may use a pool: a self-hosted pool is open to every person (their
// candidates are their own runners), and a remote-provided pool to everyone unless a use policy
// narrows it to subjects the caller matches. A use policy can only narrow.
func (c callerSubjects) permits(row store.RunnerPoolCatalogueRow) bool {
	if row.Pool.HostingType != types.RunnerPoolRemoteProvided || row.Policy == nil {
		return true
	}
	return slices.ContainsFunc(row.Policy.Subjects, c.matchesPoolSubject)
}

// poolCatalogue is the pools the caller may use, or every live pool for managers: the pool names a
// pool-use policy or the catalogue is administered over. A pool the caller may not use is absent, so it
// is indistinguishable from one that does not exist. ok=false: the answer is written.
func (s *Server) poolCatalogue(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, forUse bool) ([]store.RunnerPoolCatalogueRow, bool) {
	ctx := r.Context()
	rows, err := ps.ListRunnerPoolCatalogue(ctx, oidcHumanFromContext(ctx))
	if err != nil {
		writeServerError(w, r, "list runner pools", err)
		return nil, false
	}
	if !forUse && s.isSecurityOperator(ctx) {
		return rows, true
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		writeCeilingError(w, r, err)
		return nil, false
	}
	return slices.DeleteFunc(rows, func(row store.RunnerPoolCatalogueRow) bool { return !subj.permits(row) }), true
}

// catalogueRow finds one pool in rows.
func catalogueRow(rows []store.RunnerPoolCatalogueRow, id uuid.UUID) (store.RunnerPoolCatalogueRow, bool) {
	i := slices.IndexFunc(rows, func(row store.RunnerPoolCatalogueRow) bool { return row.Pool.ID == id })
	if i < 0 {
		return store.RunnerPoolCatalogueRow{}, false
	}
	return rows[i], true
}

// poolFromPath resolves {id} to a pool in the caller's catalogue; ok=false: the answer is written.
func (s *Server) poolFromPath(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, forUse bool) (store.RunnerPoolCatalogueRow, bool) {
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return store.RunnerPoolCatalogueRow{}, false
	}
	rows, ok := s.poolCatalogue(w, r, ps, forUse)
	if !ok {
		return store.RunnerPoolCatalogueRow{}, false
	}
	row, found := catalogueRow(rows, id)
	if !found {
		writePoolNotFound(w)
	}
	return row, found
}

// poolAvailability says whether a pool can take a run for the caller by what the catalogue knows: it is
// switched on and holds a target the caller's run could use. Whether that target is online, has room
// or passes the run's own checks is the admission fold's to decide, never read as available here.
func (s *Server) poolAvailability(row store.RunnerPoolCatalogueRow) (string, runnerpool.Reason) {
	if row.Pool.State != types.RunnerPoolActive {
		return client.RunnerPoolUnavailable, runnerpool.ReasonUnavailable
	}
	if row.Pool.HostingType == types.RunnerPoolSelfHosted && row.OwnRunners == 0 {
		return client.RunnerPoolUnavailable, runnerpool.ReasonNoEligibleMember
	}
	if row.Pool.HostingType == types.RunnerPoolRemoteProvided {
		configured := s.configuredExecutors()
		if !slices.ContainsFunc(row.ExecutorIDs, func(id string) bool { return slices.Contains(configured, id) }) {
			return client.RunnerPoolUnavailable, runnerpool.ReasonNoEligibleMember
		}
	}
	return client.RunnerPoolAvailable, ""
}

func (s *Server) handleListRunnerPools(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return
	}
	out := client.RunnerPoolList{Pools: make([]client.RunnerPoolChoice, 0, len(rows))}
	for _, row := range rows {
		availability, reason := s.poolAvailability(row)
		out.Pools = append(out.Pools, client.RunnerPoolChoice{
			ID: row.Pool.ID, Name: row.Pool.Name, HostingType: row.Pool.HostingType, Availability: availability, Reason: reason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	if row, ok := s.poolFromPath(w, r, ps, false); ok {
		writeJSON(w, http.StatusOK, row.Pool)
	}
}

// poolRevision is the pool's revision after a change, for the audit row; 0 when it cannot be read.
func poolRevision(r *http.Request, ps store.RunnerPoolStore, id uuid.UUID) int64 {
	p, err := ps.GetRunnerPool(r.Context(), id)
	if err != nil {
		return 0
	}
	return p.Revision
}

func (s *Server) auditPool(r *http.Request, action, target string, data map[string]any) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), action, target, "success", mustJSON(data)))
}
