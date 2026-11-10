// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// auditStrictPrefixes are the families strict audit covers, under /api/v1. Every POST, PUT, PATCH and
// DELETE under one is a covered change unless auditStrictExcluded names it.
var auditStrictPrefixes = []string{
	"/policies", "/governance", "/approvals", "/permissions/grants", "/drives/grants",
	"/wall-presets", "/templates", "/me/walls",
}

// auditStrictUnderPrefix reports whether route sits under one of auditStrictPrefixes, on a path-segment
// boundary (/templates covers /templates/{id}, never a /templates-something).
func auditStrictUnderPrefix(route string) bool {
	path, ok := strings.CutPrefix(route, "/api/v1")
	if !ok {
		return false
	}
	for _, p := range auditStrictPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// isAuditStrictIntent reports whether mw is (*Server).auditStrictIntent.
func isAuditStrictIntent(mw func(http.Handler) http.Handler) bool {
	name := strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name(), "-fm")
	return strings.HasSuffix(name, ".auditStrictIntent")
}

// TestAuditStrictCensus walks the built router (maximally configured, as the authz matrix builds it):
// every change under a covered prefix is in auditStrictRoutes or auditStrictExcluded and never both,
// every covered route has auditStrictIntent in its middleware chain, and neither table names a route
// the router does not have.
func TestAuditStrictCensus(t *testing.T) {
	seen := map[string]bool{}
	for _, srv := range []*Server{func() *Server { s, _, _, _ := newAuthzMatrixServer(t); return s }(), newAuthzMatrixServerWithUI(t)} {
		err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
			key := method + " " + route
			seen[key] = true
			switch method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				if auditStrictRoutes[key] || auditStrictExcluded[key] {
					t.Errorf("%s is not a change but is listed in a strict-audit table", key)
				}
				return nil
			}
			covered, excluded := auditStrictRoutes[key], auditStrictExcluded[key]
			under := auditStrictUnderPrefix(route)
			switch {
			case covered && excluded:
				t.Errorf("%s is both covered and excluded", key)
			case under && !covered && !excluded:
				t.Errorf("%s changes something under a strict-audit prefix but is in neither auditStrictRoutes nor auditStrictExcluded — cover it, or exclude it with why it changes nothing", key)
			case (covered || excluded) && !under:
				t.Errorf("%s is in a strict-audit table but under none of %v", key, auditStrictPrefixes)
			}
			if covered && !hasAuditStrictIntent(mws) {
				t.Errorf("%s is covered but auditStrictIntent is not in its middleware chain, so no intent row is written", key)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("chi.Walk: %v", err)
		}
	}
	for _, table := range []map[string]bool{auditStrictRoutes, auditStrictExcluded} {
		for key := range table {
			if !seen[key] {
				t.Errorf("%s is in a strict-audit table but the router has no such route", key)
			}
		}
	}
	if len(auditStrictRoutes) < 40 || len(seen) < 100 {
		t.Fatalf("covered %d of %d walked routes — the walk or the table broke, not the census", len(auditStrictRoutes), len(seen))
	}
}

func hasAuditStrictIntent(mws []func(http.Handler) http.Handler) bool {
	for _, mw := range mws {
		if isAuditStrictIntent(mw) {
			return true
		}
	}
	return false
}

// TestAuditStrictCensusSeesAMissingRow: the census's own rule fires on a change under a prefix that is in
// neither table, so a new route cannot pass it by being invisible to it.
func TestAuditStrictCensusSeesAMissingRow(t *testing.T) {
	key := "PUT /api/v1/governance/units/{id}"
	if !auditStrictRoutes[key] || !auditStrictUnderPrefix("/api/v1/governance/units/{id}") {
		t.Fatalf("%s should be covered and under a prefix", key)
	}
	if auditStrictUnderPrefix("/api/v1/templates-other") || auditStrictUnderPrefix("/api/v1/runs/{id}") || !auditStrictUnderPrefix("/api/v1/me/walls") {
		t.Error("auditStrictUnderPrefix does not match on a path-segment boundary")
	}
	srv, _, _, _ := newAuthzMatrixServer(t)
	found := false
	_ = chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if method+" "+route == key {
			found = hasAuditStrictIntent(mws)
		}
		return nil
	})
	if !found {
		t.Errorf("%s has no auditStrictIntent in the middleware chain chi.Walk reports", key)
	}
}
