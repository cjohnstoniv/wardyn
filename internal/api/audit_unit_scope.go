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

// unitAdminAuditRead is whether a unit admin reads, on GET /audit and its export, the rows of the runs
// frozen to a unit in their subtrees. On by default: a grant already conveys governing that team, and the
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
// auditScope did not settle as their own run: nil, true for no scope (the member collapse follows); a
// scope when they hold a unit-admin grant and either asked for no run or asked for a run frozen to one of
// their units (run is that run as auditScope read it, zero for none or an unknown id). ok false means
// the 503 is written. A grant only a stale or truncated group snapshot would vouch for counts as none
// (unitAdminSubjects), as do the callers no admin tier answers for.
func (s *Server) unitAuditScopeFor(w http.ResponseWriter, r *http.Request, runAsked bool, run types.AgentRun) (*unitAuditScope, bool) {
	reader, ok := s.cfg.Store.(store.UnitAuditReader)
	if !unitAdminAuditRead || !ok {
		return nil, true
	}
	ctx := r.Context()
	grants, err := s.callerUnitGrants(ctx)
	if err == nil && len(grants) == 0 {
		return nil, true
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
		// As for the ownership read: a scope that cannot be read is not "nothing to show".
		slog.ErrorContext(ctx, "wardyn: audit scope could not read the unit-admin scope", slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAuditScopeUnavailable, "the audit scope could not be determined; try again")
		return nil, false
	}
	in, _ := subtreeUnits(rows, grants, limit)
	if runAsked && (run.UnitID == nil || !in[*run.UnitID]) {
		return nil, true
	}
	units := slices.Collect(maps.Keys(in))
	if len(units) == 0 {
		return nil, true
	}
	return &unitAuditScope{units: units, reader: reader}, true
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

// exportPages is the export's page read for the unit feed: by position, as the whole log's export reads.
func (scope *unitAuditScope) exportPages(r *http.Request, filter store.AuditFilter) func(after int64, offset int) ([]types.AuditEvent, int64, error) {
	return func(after int64, _ int) ([]types.AuditEvent, int64, error) {
		return scope.reader.QueryAuditEventsForUnitsAfter(r.Context(), scope.units, filter, after, auditExportPageSize)
	}
}
