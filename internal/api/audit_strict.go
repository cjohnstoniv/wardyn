// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// Strict audit (WARDYN_AUDIT_STRICT, Config.AuditStrict): with strict mode on, a covered change is
// refused if its audit intent row cannot be written first. The intent row (audit.intent.record) is
// written straight to the store through a recorder with no spool, before the handler runs; when it
// cannot be written the request is refused 503 audit_unavailable and the handler never runs. The
// handler's own detailed row is unchanged: it is written after the change, and can still spool.

// auditStrictRoutes is every covered change, keyed as chi.Walk reports a route: each POST, PUT, PATCH
// and DELETE under /policies, /governance, /approvals, /permissions, /drives/grants,
// /wall-presets, /templates and /me/walls, less auditStrictExcluded. Wall presets, templates and a
// person's own walls carry policy, so they are covered. TestAuditStrictCensus fails on a change under
// those prefixes that is in neither table, and on a covered route auditStrictIntent does not wrap.
var auditStrictRoutes = map[string]bool{
	"POST /api/v1/policies":        true,
	"PUT /api/v1/policies/{id}":    true,
	"DELETE /api/v1/policies/{id}": true,

	"POST /api/v1/governance/units":                       true,
	"PUT /api/v1/governance/units/{id}":                   true,
	"DELETE /api/v1/governance/units/{id}":                true,
	"POST /api/v1/governance/units/{id}/admins":           true,
	"DELETE /api/v1/governance/units/{id}/admins/{admin}": true,
	"PUT /api/v1/governance/units/{id}/deciders":          true,
	"PUT /api/v1/governance/units/{id}/envelope":          true,
	"DELETE /api/v1/governance/units/{id}/envelope":       true,
	"POST /api/v1/governance/memberships":                 true,
	"PUT /api/v1/governance/memberships/{id}":             true,
	"DELETE /api/v1/governance/memberships/{id}":          true,
	"POST /api/v1/governance/memberships/{id}/promote":    true,
	"POST /api/v1/governance/changes/{id}/approve":        true,
	"POST /api/v1/governance/changes/{id}/reject":         true,
	"PUT /api/v1/governance/changes/{id}/note":            true,
	"POST /api/v1/governance/requests":                    true,
	"POST /api/v1/governance/requests/{id}/approve":       true,
	"POST /api/v1/governance/requests/{id}/reject":        true,
	"PUT /api/v1/governance/egress-baseline":              true,
	"PUT /api/v1/governance/boundary":                     true,
	"PUT /api/v1/governance/recording":                    true,
	"POST /api/v1/governance/restrictions":                true,
	"PUT /api/v1/governance/restrictions/{id}":            true,
	"DELETE /api/v1/governance/restrictions/{id}":         true,
	"PUT /api/v1/governance/approval-rules/{name}":        true,
	"DELETE /api/v1/governance/approval-rules/{name}":     true,
	"PUT /api/v1/governance/credential-delivery":          true,

	"POST /api/v1/approvals/{id}/approve":   true,
	"POST /api/v1/approvals/{id}/deny":      true,
	"POST /api/v1/approvals/{id}/take-over": true,
	"POST /api/v1/approvals/{id}/hand-back": true,
	"POST /api/v1/approvals/{id}/decline":   true,

	"POST /api/v1/permissions/grants":               true,
	"DELETE /api/v1/permissions/grants/{id}":        true,
	"PUT /api/v1/permissions/enforcement":           true,
	"PUT /api/v1/permissions/availability/{kind}/*": true,
	"POST /api/v1/drives/grants":                    true,
	"DELETE /api/v1/drives/grants/{id}":             true,

	"POST /api/v1/wall-presets":        true,
	"PUT /api/v1/wall-presets/{id}":    true,
	"DELETE /api/v1/wall-presets/{id}": true,

	"POST /api/v1/templates":           true,
	"POST /api/v1/templates/import":    true,
	"PUT /api/v1/templates/{id}":       true,
	"DELETE /api/v1/templates/{id}":    true,
	"POST /api/v1/templates/{id}/copy": true,

	"PUT /api/v1/me/walls":    true,
	"DELETE /api/v1/me/walls": true,
}

// auditStrictExcluded is the POSTs under those prefixes that change nothing: each reads, works an answer
// out and returns it.
var auditStrictExcluded = map[string]bool{
	"POST /api/v1/policies/grade":                    true,
	"POST /api/v1/governance/check":                  true,
	"POST /api/v1/governance/dry-run":                true,
	"POST /api/v1/governance/approval-rules/dry-run": true,
}

// strictIntentBodyCap bounds the body read to hash it. It is above every covered handler's own cap (the
// template import's, 2 MiB and a little), so a body past it is one its handler would refuse too.
const strictIntentBodyCap = 4 << 20

// auditUnavailableMsg answers a covered change whose intent row could not be written.
const auditUnavailableMsg = "Strict audit is on and this change's audit intent row could not be written, so nothing was changed. Try again."

// auditStrictIntent is the authenticated group's middleware for strict mode. chi has resolved the route
// before a group's middleware runs, so the route pattern names the change.
func (s *Server) auditStrictIntent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		if s.cfg.AuditStrict == nil || rctx == nil || !auditStrictRoutes[r.Method+" "+rctx.RoutePattern()] {
			next.ServeHTTP(w, r)
			return
		}
		var body []byte
		if r.Body != nil {
			var ok bool
			if body, ok = readCappedBody(w, r, strictIntentBodyCap, "request body"); !ok {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if !s.recordStrictIntent(r, rctx, body) {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonAuditUnavailable, auditUnavailableMsg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recordStrictIntent writes the change's audit.intent.record row through the strict recorder, and
// reports whether the store took it. The row names the change, never its body: the method, the
// resolved path, the route, the path's parameters, the request id and the body's SHA-256. The actor is
// the caller, as on the handler's own row. The path, each parameter and the request id come from the
// caller, who may not even hold the route's role yet, so each is cut to the audit target's cap
// (store.CapAuditTarget): the route pattern names the change, and the cut value still names the target.
func (s *Server) recordStrictIntent(r *http.Request, rctx *chi.Context, body []byte) bool {
	ctx := r.Context()
	params := map[string]string{}
	for i, k := range rctx.URLParams.Keys {
		if k != "*" && i < len(rctx.URLParams.Values) {
			params[k] = store.CapAuditTarget(rctx.URLParams.Values[i])
		}
	}
	// A route ending in a wildcard names its target there (an image ref, with its slashes); the mount's
	// own "*" entries are routing, not a parameter.
	route := rctx.RoutePattern()
	if strings.HasSuffix(route, "/*") {
		params["*"] = store.CapAuditTarget(rctx.URLParam("*"))
	}
	sum := sha256.Sum256(body)
	at, actor := actorFromRequest(r)
	path := store.CapAuditTarget(r.URL.Path)
	ev := s.stampAuditEvent(ctx, s.auditEvent(nil, at, actor, "audit.intent.record", path, "success", mustJSON(map[string]any{
		"method":      r.Method,
		"path":        path,
		"route":       route,
		"params":      params,
		"request_id":  store.CapAuditTarget(middleware.GetReqID(ctx)),
		"body_sha256": hex.EncodeToString(sum[:]),
	})))
	if err := s.cfg.AuditStrict.Record(ctx, ev); err != nil {
		slog.ErrorContext(ctx, "wardynd: strict audit: the intent row could not be written; the change is refused",
			"method", r.Method, "route", route, "err", err)
		return false
	}
	return true
}
