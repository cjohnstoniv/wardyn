// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// controlPlaneCall is one proxy call to wardynd's internal listener, made
// against base (a control-plane URL with a trailing slash).
type controlPlaneCall struct {
	name string
	call func(t *testing.T, base string, cp *httptest.Server)
	want string
}

// checkControlPlanePaths runs each call against a fake internal listener whose
// URL is configured with a trailing slash, and checks the path it reached.
func checkControlPlanePaths(t *testing.T, calls []controlPlaneCall) {
	t.Helper()
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
			}))
			defer cp.Close()
			tc.call(t, cp.URL+"/", cp)
			mu.Lock()
			defer mu.Unlock()
			if len(paths) != 1 || paths[0] != tc.want {
				t.Fatalf("control plane saw %q, want [%q]", paths, tc.want)
			}
		})
	}
}

// Characterization: renew, activity and the brokered local routes already
// trim a trailing slash from the control-plane URL.
func TestControlPlaneTrailingSlashTrimmedPaths(t *testing.T) {
	id := uuid.New()
	checkControlPlanePaths(t, []controlPlaneCall{
		{"token renew", func(t *testing.T, base string, cp *httptest.Server) {
			_, _, _ = renewToken(context.Background(), base, "tok", cp.Client())
		}, "/api/v1/internal/token/renew"},
		{"activity", func(t *testing.T, base string, cp *httptest.Server) {
			reportActivity(context.Background(), newTokenSource("tok"), base, cp.Client())
		}, "/api/v1/internal/activity"},
		{"local approval route", func(t *testing.T, base string, cp *httptest.Server) {
			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080/", "RUNTOK", upstreamAddr(cp), nil, nil)
			p.ServeHTTP(httptest.NewRecorder(), mustLocalReq(t, http.MethodGet, routeApprovals+id.String(), nil))
		}, "/api/v1/internal/approvals/" + id.String()},
	})
}

// A trailing slash on the control-plane URL passes boot validation, so every
// call built from it must reach the same path as without one.
func TestControlPlaneTrailingSlashReachesTheInternalRoutes(t *testing.T) {
	id := uuid.New()
	checkControlPlanePaths(t, []controlPlaneCall{
		{"decision post", func(t *testing.T, base string, cp *httptest.Server) {
			_ = newDecisionSink(base, newTokenSource("tok"), 1, cp.Client(), nil).post(egress.DecisionLog{})
		}, "/api/v1/internal/decisions"},
		{"approval raise", func(t *testing.T, base string, cp *httptest.Server) {
			_, _ = newApprovalClient(base, newTokenSource("tok"), uuid.New(), cp.Client()).raiseBytes(context.Background(), []byte("{}"))
		}, "/api/v1/internal/approvals"},
		{"approval fetch", func(t *testing.T, base string, cp *httptest.Server) {
			_, _ = newApprovalClient(base, newTokenSource("tok"), uuid.New(), cp.Client()).fetch(context.Background(), id)
		}, "/api/v1/internal/approvals/" + id.String()},
		{"approval read during a hold", func(t *testing.T, base string, cp *httptest.Server) {
			_, _, _ = httpApprovalReader{base: base, token: newTokenSource("tok"), client: cp.Client()}.readApproval(context.Background(), id)
		}, "/api/v1/internal/approvals/" + id.String()},
		{"injection resolve", func(t *testing.T, base string, cp *httptest.Server) {
			_, _ = resolveInjectionQuery(context.Background(), base, "tok", id, nil, cp.Client())
		}, "/api/v1/internal/injection/" + id.String()},
	})
}
