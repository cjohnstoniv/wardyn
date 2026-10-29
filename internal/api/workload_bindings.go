// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Workload bindings: "the custom resources of namespace N, on the runners of pool P, launch as service
// identity S". A binding is standing authority over S, so it is the security tier's to make, applied
// directly and audited as a token move is, and a signed-in person's alone: no API token makes one. It
// records S's sponsor; a new sponsor, a suspended or retired S, or the sponsor's cut sessions end it as
// authority (the custom-resource path checks), and retiring S deletes it.

// workloadNamespaceRE is a Kubernetes namespace: a DNS label (the schema's own check).
var workloadNamespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

const workloadBindingNeedsPersonMsg = "A binding is standing authority over a service identity: a signed-in person makes it, never an API token."

func workloadBindingOut(b store.WorkloadBinding) client.WorkloadBinding {
	return client.WorkloadBinding{ID: b.ID, PoolID: b.PoolID, Namespace: b.Namespace, Service: b.Service, Sponsor: b.Sponsor,
		CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt}
}

// workloadBindingAuditData is a workload_binding.* row's data.
func workloadBindingAuditData(b store.WorkloadBinding) []byte {
	return mustJSON(map[string]any{"pool": b.PoolID, "namespace": b.Namespace, "service": b.Service, "sponsor": b.Sponsor})
}

// handleCreateWorkloadBinding is POST /workload-bindings {pool, namespace, service}: 201 with the binding.
func (s *Server) handleCreateWorkloadBinding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if oidcHumanFromContext(ctx) == "" || apiTokenIDFromContext(ctx) != uuid.Nil || delegatedRequest(ctx) {
		writeErrorReason(w, http.StatusForbidden, reasonWorkloadBindingNeedsPerson, workloadBindingNeedsPersonMsg)
		return
	}
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	var req client.CreateWorkloadBindingRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Pool == "" || req.Service == "" || !workloadNamespaceRE.MatchString(req.Namespace) {
		writeErrorReason(w, http.StatusBadRequest, reasonWorkloadBindingInvalid,
			"pool and service are required, and namespace must be a Kubernetes namespace name (a lowercase DNS label)")
		return
	}
	poolID, ok, err := bindingPoolID(ctx, ps, req.Pool)
	switch {
	case err != nil:
		writeServerError(w, r, "read runner pools", err)
		return
	case !ok:
		writePoolNotFound(w)
		return
	}
	si, err := ss.GetServiceIdentity(ctx, req.Service)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonServiceIdentityNotFound, "There is no service identity of that name.")
		return
	case err != nil:
		writeServerError(w, r, "read service identity", err)
		return
	case !si.Usable():
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "The service identity is suspended or retired.")
		return
	}
	b, err := ws.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: poolID, Namespace: req.Namespace, Service: si.Name, Sponsor: si.Sponsor,
		CreatedBy: principalFromRequest(r)})
	switch {
	case errors.Is(err, store.ErrWorkloadBindingConflict):
		writeErrorReason(w, http.StatusConflict, reasonWorkloadBindingConflict, "That namespace is bound on this pool already. Delete that binding first.")
		return
	case errors.Is(err, store.ErrNotFound):
		writePoolNotFound(w)
		return
	case err != nil:
		writeServerError(w, r, "create workload binding", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "workload_binding.create", b.ID.String(), "success", workloadBindingAuditData(b)))
	writeJSON(w, http.StatusCreated, workloadBindingOut(b))
}

// bindingPoolID resolves a binding's pool: an id as given (the store refuses an unknown or deleted one), or
// the name of a live organization pool. ok is false when no pool has that name.
func bindingPoolID(ctx context.Context, ps store.RunnerPoolStore, ref string) (uuid.UUID, bool, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return id, true, nil
	}
	rows, err := ps.ListRunnerPoolCatalogue(ctx, "", false)
	if err != nil {
		return uuid.Nil, false, err
	}
	for _, row := range rows {
		if row.Pool.Scope == types.RunnerPoolOrganization && strings.EqualFold(row.Pool.Name, ref) {
			return row.Pool.ID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// handleListWorkloadBindings is GET /workload-bindings: every binding, by pool then namespace.
func (s *Server) handleListWorkloadBindings(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	servePage(w, r, page, func(p store.Page) ([]client.WorkloadBinding, error) {
		rows, err := ws.ListWorkloadBindings(r.Context(), p)
		out := make([]client.WorkloadBinding, 0, len(rows))
		for _, b := range rows {
			out = append(out, workloadBindingOut(b))
		}
		return out, err
	}, nil)
}

// handleDeleteWorkloadBinding is DELETE /workload-bindings/{id}: 204. The runs it launched continue, and
// its namespace's custom resources stop launching.
func (s *Server) handleDeleteWorkloadBinding(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "workload binding")
	if !ok {
		return
	}
	b, err := ws.DeleteWorkloadBinding(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadBindingNotFound, "There is no workload binding with that id.")
		return
	case err != nil:
		writeServerError(w, r, "delete workload binding", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "workload_binding.delete", b.ID.String(), "success", workloadBindingAuditData(b)))
	w.WriteHeader(http.StatusNoContent)
}
