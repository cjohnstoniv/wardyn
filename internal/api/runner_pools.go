// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The runner pool routes (0.9). A pool is a label for a choice and never authority: choosing one grants
// no runner, credential, drive or confinement class. Organization pools are the organisation's and hold
// remote-hosted runners only; each user pool is one person's own, holding that person's claimed
// self-hosted runners and the remote-hosted runners whose people policy admits them.
//
//	catalogue reads, own user pools and their runners,          member (the caller's own data only)
//	personal default
//	organization pool create/update/delete, remote-hosted       admin  (SUPER)
//	membership, org default; remote-hosted runner name
//	pool-use policy (organization pools only), remote-hosted    security (admin or security_admin);
//	runner people policy, remote-hosted runner reads            the two policies are held for a second
//	                                                            human under WARDYN_GOVERNANCE_SECOND_HUMAN
//
// Only a user pool's owner writes it or uses it: another person's pool answers every verb as an
// unknown id does, and operators list every pool read-only for oversight. No route here takes an
// owner, and none may claim, transfer or dispatch another person's runner. A store without pool storage
// answers every route 501 runner_pools_unavailable.
func (s *Server) mountRunnerPoolRoutes(r, operatorOnly, securityOps chi.Router) {
	r.Get("/runner-pools", s.handleListRunnerPools)
	r.Get("/me/runner-pools", s.handleListMyRunnerPools)
	r.Get("/runner-pools/{id}", s.handleGetRunnerPool)
	r.Get("/runner-pools/{id}/members", s.handleListRunnerPoolMembers)
	operatorOnly.Post("/runner-pools", s.handleCreateRunnerPool)
	operatorOnly.Put("/runner-pools/{id}", s.handleUpdateRunnerPool)
	operatorOnly.Delete("/runner-pools/{id}", s.handleDeleteRunnerPool)
	operatorOnly.Put("/runner-pools/{id}/remote-runners/{rid}", s.handleAddRunnerPoolExecutor)
	operatorOnly.Delete("/runner-pools/{id}/remote-runners/{rid}", s.handleRemoveRunnerPoolExecutor)
	operatorOnly.Put("/runner-pools/{id}/runners/{runner}", s.handleAddRunnerPoolOrgRunner)
	operatorOnly.Delete("/runner-pools/{id}/runners/{runner}", s.handleRemoveRunnerPoolOrgRunner)
	r.Post("/me/runner-pools", s.handleCreateMyRunnerPool)
	r.Put("/me/runner-pools/{id}", s.handleUpdateMyRunnerPool)
	r.Delete("/me/runner-pools/{id}", s.handleDeleteMyRunnerPool)
	r.Put("/me/runner-pools/{id}/runners/{runner}", s.handleAddMyRunnerToPool)
	r.Delete("/me/runner-pools/{id}/runners/{runner}", s.handleRemoveMyRunnerFromPool)
	r.Put("/me/runner-pools/{id}/remote-runners/{rid}", s.handleAddMyRemoteRunnerToPool)
	r.Delete("/me/runner-pools/{id}/remote-runners/{rid}", s.handleRemoveMyRemoteRunnerFromPool)
	securityOps.Get("/remote-runners", s.handleListRemoteRunners)
	securityOps.Get("/remote-runners/{rid}", s.handleGetRemoteRunner)
	operatorOnly.Put("/remote-runners/{rid}", s.handleUpdateRemoteRunner)
	securityOps.Put("/remote-runners/{rid}/people-policy", s.handlePutRemoteRunnerPeoplePolicy)
	securityOps.Delete("/remote-runners/{rid}/people-policy", s.handleDeleteRemoteRunnerPeoplePolicy)
	r.Get("/me/remote-runners", s.handleListMyRemoteRunners)
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

// permits reports whether the caller may use a pool: a user pool only when owner (poolOwner, the
// signed-in person) is its owner, and an organization pool to everyone unless a use policy narrows it
// to subjects the caller matches. A use policy can only narrow.
func (c callerSubjects) permits(row store.RunnerPoolCatalogueRow, owner string) bool {
	if row.Pool.Scope == types.RunnerPoolUser {
		return owner != "" && row.Pool.Owner == owner
	}
	if row.Policy == nil {
		return true
	}
	return slices.ContainsFunc(row.Policy.Subjects, c.matchesPoolSubject)
}

// poolCatalogue is the pools the caller may use. With forUse false a manager (security admin or admin)
// gets every live pool instead, every person's user pools included, read-only for oversight:
// that view is for display and management only and is never a statement of what the caller may use, so
// anything that admits, defaults or offers a choice passes forUse true. A pool the caller may not use is
// absent, so it is indistinguishable from one that does not exist. ok=false: the answer is written.
func (s *Server) poolCatalogue(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, forUse bool) ([]store.RunnerPoolCatalogueRow, bool) {
	rows, refusal := s.permittedPools(r.Context(), ps, forUse)
	return rows, !refusal.write(s, w, r)
}

// permittedPools is poolCatalogue's read, answering with a refusal instead of writing it, for the
// run fold: the same catalogue, the same narrowing, so a launch sees exactly the pools the list shows.
func (s *Server) permittedPools(ctx context.Context, ps store.RunnerPoolStore, forUse bool) ([]store.RunnerPoolCatalogueRow, *runRefusal) {
	owner := poolOwner(ctx)
	oversight := !forUse && s.isSecurityOperator(ctx)
	rows, err := ps.ListRunnerPoolCatalogue(ctx, owner, oversight)
	if err != nil {
		return nil, runServerError("list runner pools", err)
	}
	if oversight {
		return rows, nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return nil, ceilingRunRefusal(err)
	}
	return slices.DeleteFunc(rows, func(row store.RunnerPoolCatalogueRow) bool { return !subj.permits(row, owner) }), nil
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

// poolState is one permitted pool's availability for the caller, and the caller's own candidates
// when the checks reached a user pool's runners.
type poolState struct {
	availability string
	reason       runnerpool.Reason
	own          []poolTarget
}

func (st poolState) choice(row store.RunnerPoolCatalogueRow) client.RunnerPoolChoice {
	return client.RunnerPoolChoice{ID: row.Pool.ID, Name: row.Pool.Name, Scope: row.Pool.Scope, Availability: st.availability, Reason: st.reason}
}

// poolAvailability says whether a pool can take a run for the caller: the first of
// runnerpool.AvailabilityReasons that applies, in that order, by the catalogue, the caller's
// deny_placements, the runner switch and the runners themselves. It judges each kind of runner in the
// pool as placement does (poolTargets, kindTargets): deny holds each kind, a user pool's remote-hosted
// runners count only while their people policy admits the caller, and the pool is available while one
// kind that is not denied can take a run. The pool list and the dry doors both ask it, so they agree.
// Whether a candidate has room or passes the run's own checks is the admission fold's to decide.
func (s *Server) poolAvailability(ctx context.Context, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow, denied []types.Placement) (poolState, *runRefusal) {
	unavailable := func(r runnerpool.Reason) (poolState, *runRefusal) {
		return poolState{availability: client.RunnerPoolUnavailable, reason: r}, nil
	}
	remoteDenied := slices.Contains(denied, types.PlacementRemote)
	switch {
	case row.Pool.State != types.RunnerPoolActive:
		return unavailable(runnerpool.ReasonUnavailable)
	case row.Pool.Scope == types.RunnerPoolOrganization && remoteDenied:
		return unavailable(runnerpool.AvailabilityPlacementDenied)
	case row.Pool.Scope == types.RunnerPoolOrganization:
		if !s.anyConfigured(row) {
			return unavailable(runnerpool.ReasonNoEligibleMember)
		}
		return poolState{availability: client.RunnerPoolAvailable}, nil
	}
	admitted, refusal := s.admittedRemoteMembers(ctx, row)
	if refusal != nil {
		return poolState{}, refusal
	}
	remote, own := s.anyConfigured(admitted), row.OwnRunners > 0
	localDenied := slices.Contains(denied, types.PlacementLocal)
	st, refusal := s.ownRunnerAvailability(ctx, ps, row)
	switch {
	case refusal != nil:
		return poolState{}, refusal
	case remote && !remoteDenied:
		st.availability, st.reason = client.RunnerPoolAvailable, ""
	case (own || remote) && (!own || localDenied) && (!remote || remoteDenied):
		st.availability, st.reason = client.RunnerPoolUnavailable, runnerpool.AvailabilityPlacementDenied
	}
	return st, nil
}

// anyConfigured reports whether one of the row's remote-hosted members is a runner this deployment runs.
func (s *Server) anyConfigured(row store.RunnerPoolCatalogueRow) bool {
	configured := s.configuredExecutors()
	return slices.ContainsFunc(row.ExecutorIDs, func(id string) bool { return slices.Contains(configured, id) })
}

// ownRunnerAvailability is a user pool's availability by the caller's OWN runners in it alone
// (ownRunnerTargets): available while runners are switched on and one of them is online. It also
// answers those runners, for the pool's main-drive facts.
func (s *Server) ownRunnerAvailability(ctx context.Context, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) (poolState, *runRefusal) {
	unavailable := func(r runnerpool.Reason) (poolState, *runRefusal) {
		return poolState{availability: client.RunnerPoolUnavailable, reason: r}, nil
	}
	switch err := s.runnersEnabled(ctx); {
	case errors.Is(err, errRunnersDisabled):
		return unavailable(runnerpool.AvailabilityPlacementUnavailable)
	case err != nil:
		return poolState{availability: client.RunnerPoolUnknown}, nil
	}
	if row.OwnRunners == 0 {
		return unavailable(runnerpool.ReasonNoEligibleMember)
	}
	own, refusal := s.ownRunnerTargets(ctx, &createRunRequest{}, ps, row)
	if refusal != nil {
		if refusal.status >= http.StatusInternalServerError {
			return poolState{}, refusal
		}
		return unavailable(runnerpool.Reason(refusal.body.Reason))
	}
	st := poolState{availability: client.RunnerPoolAvailable, own: own}
	if !slices.ContainsFunc(own, func(t poolTarget) bool { return s.runnerOnline(*t.runnerID) }) {
		st.availability, st.reason = client.RunnerPoolUnavailable, runnerpool.AvailabilityNoOnlineRunner
	}
	return st, nil
}

func (s *Server) handleListRunnerPools(w http.ResponseWriter, r *http.Request) {
	s.listRunnerPools(w, r, false)
}

// handleListMyRunnerPools is GET /me/runner-pools: the same list as GET /runner-pools, but only the
// pools the caller may use, so an administrator sees no one else's user pools here.
func (s *Server) handleListMyRunnerPools(w http.ResponseWriter, r *http.Request) {
	s.listRunnerPools(w, r, true)
}

func (s *Server) listRunnerPools(w http.ResponseWriter, r *http.Request, forUse bool) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	rt := types.RunnerPoolRunType(r.URL.Query().Get("run_type"))
	if rt != "" && !rt.Valid() {
		writePoolInvalid(w, "run_type is background or interactive.")
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, forUse)
	if !ok {
		return
	}
	// Only the oversight read can pass the cap: it reads one row more, so the list can say more exist.
	if len(rows) > store.RunnerPoolsOversightMax {
		rows = rows[:store.RunnerPoolsOversightMax]
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	subj, err := s.callerSubjects(r.Context())
	if err != nil {
		writeCeilingError(w, r, err)
		return
	}
	ceiling, err := s.effectiveCeiling(r.Context())
	if err != nil {
		writeCeilingError(w, r, err)
		return
	}
	list, refusal := s.poolList(r, ps, rows, subj, ceiling, rt)
	if refusal.write(s, w, r) {
		return
	}
	writeJSON(w, http.StatusOK, list)
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

func (s *Server) auditPool(r *http.Request, action, target string, data map[string]any) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), action, target, "success", mustJSON(data)))
}
