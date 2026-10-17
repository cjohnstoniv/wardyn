// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A run's grant list is the same scope under another door, read by the same
// person: it is served without the name to them and whole to the security tier.
func TestListGrants_AMemberNeverReadsASharedSecretsName(t *testing.T) {
	run := launchSharedRun(t)
	path := "/api/v1/runs/" + run.id.String() + "/grants"
	get := func(tier kernelTier) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		tier.auth(t, run.f.st.govEscapeStore, []string{"eng"}, r)
		w := httptest.NewRecorder()
		panicFails(t, run.f.srv.Handler()).ServeHTTP(w, r)
		return w
	}
	w := get(kernelUserTier)
	if body := w.Body.String(); w.Code != http.StatusOK || strings.Contains(body, compOperatorSecret) ||
		!strings.Contains(body, `"scope":{"host":"org-api.example","require_tls":true,"shared":true}`) {
		t.Errorf("GET %s as the run's owner = %d %s, want the shared grant without the organisation's secret name", path, w.Code, body)
	}
	w = get(kernelTiers[0])
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, string(run.grant.Spec.Scope)) {
		t.Errorf("GET %s as an admin = %d %s, want the grant's scope whole", path, w.Code, body)
	}
}
