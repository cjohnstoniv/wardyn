// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitAdminAuditRead is whether a unit admin reads, on GET /audit alone (the export is not theirs), the rows
// of the runs frozen to a unit in their subtrees. On by default: a grant already conveys governing that team, and the
// unit view (audit_unit_view.go) serves only allow-listed fields. Set it false and a unit admin reads the
// audit log as any member does.
const unitAdminAuditRead = true

// unitAuditScope is what auditScope hands a unit admin: the units of their subtrees, read per request so a
// revoked grant or a unit moved out stops serving on the next page, and the store's unit read.
type unitAuditScope struct {
	units  []uuid.UUID
	reader store.UnitAuditReader
}

// unitAuditScopeFor is the caller's unit scope, for a caller below the security tier whose request
// auditScope did not settle as their own run: nil for no scope (the member collapse follows); a scope
// when they hold a unit-admin grant and either asked for no run or asked for a run frozen to one of their
// units (run is that run as auditScope read it, zero for none or an unknown id). A grant only a stale or
// truncated group snapshot would vouch for counts as none (unitAdminSubjects), as do the callers no admin
// tier answers for. A grant, tree or depth read that fails is also no scope, logged: the caller gets the
// member's empty 200, the answer the design keeps for every request it does not serve.
func (s *Server) unitAuditScopeFor(r *http.Request, runAsked bool, run types.AgentRun) *unitAuditScope {
	reader, ok := s.cfg.Store.(store.UnitAuditReader)
	if !unitAdminAuditRead || !ok {
		return nil
	}
	ctx := r.Context()
	grants, err := s.callerUnitGrants(ctx)
	if err == nil && len(grants) == 0 {
		return nil
	}
	var rows []types.GovernanceUnit
	limit := 0
	if err == nil {
		rows, err = s.unitTreeRows(ctx)
	}
	if err == nil {
		limit, err = s.unitDepthLimit(ctx)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardyn: audit scope could not read the unit-admin scope; serving the member view", slog.Any("err", err))
		return nil
	}
	in, _ := subtreeUnits(rows, grants, limit)
	if runAsked && (run.UnitID == nil || !in[*run.UnitID]) {
		return nil
	}
	units := slices.Collect(maps.Keys(in))
	if len(units) == 0 {
		return nil
	}
	return &unitAuditScope{units: units, reader: reader}
}

// unitTargetFilterOK is whether a unit admin's ?target= can be applied, and the 400 when it cannot: only
// with an action or action_prefix in a family whose target the unit view serves (unitViewTargetFamilies).
// Anywhere else the filter would match a target the view serves empty, and rows coming back would tell
// the caller that the target exists. The refusal does not depend on what is stored.
func unitTargetFilterOK(w http.ResponseWriter, filter store.AuditFilter) bool {
	if filter.Target == "" || unitViewServesTarget(filter.Action) || unitViewServesTarget(filter.ActionPrefix) {
		return true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonAuditUnitTargetFilter,
		"a unit admin's target filter needs an action or action_prefix in run., egress. or approval.")
	return false
}

// errUnitAuditNoFallback answers the fetch-all fallback servePage takes only without a page reader,
// which a unit scope always has: the unit feed is never served from the whole log.
var errUnitAuditNoFallback = errors.New("api: the unit audit feed has no unpaged read")

// serveUnitAuditFeed is GET /audit for a unit admin who named no run: the rows of their units' runs,
// newest first, under the global feed's default cap, filtered and paged as the feed is.
func (s *Server) serveUnitAuditFeed(w http.ResponseWriter, r *http.Request, scope *unitAuditScope, filter store.AuditFilter) {
	page, ok := parseListPage(w, r, auditGlobalDefaultLimit)
	if !ok {
		return
	}
	read := func(p store.Page) ([]types.AuditEvent, error) {
		return scope.reader.QueryAuditEventsForUnits(r.Context(), scope.units, filter, p)
	}
	servePage(w, r, page, servedPage(s.unitAuditRows(r), read), func() ([]types.AuditEvent, error) {
		return nil, errUnitAuditNoFallback
	})
}
