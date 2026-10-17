// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestRecordingDenyCarriesTheOrgReason: a refused upload's brokered:recording row says why the
// organisation refused it, and a body with no reason token (or prose in its place) leaves none.
func TestRecordingDenyCarriesTheOrgReason(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"recording off", `{"error":"this organisation keeps no session recordings","reason":"recording_off"}`, "recording_off", http.StatusConflict},
		{"relay refused", `{"error":"this route is not served through a runner's relay","reason":"relay_route_refused"}`, "relay_route_refused", http.StatusForbidden},
		{"no reason", `oops`, "", http.StatusInternalServerError},
		{"prose reason", `{"reason":"Not A Token"}`, "", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer cp.Close()
			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPut, routeRecordings+uuid.New().String(), strings.NewReader("{}\n")))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d passed through", rec.Code, tc.status)
			}
			d := lastDecision(t, buf)
			if d.RuleSource != ruleSourceRecordings || d.Decision != egress.Deny || d.Cause != tc.want {
				t.Fatalf("decision = %+v, want a brokered:recording deny with cause %q", d, tc.want)
			}
		})
	}
}
