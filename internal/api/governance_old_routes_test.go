// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
)

// The 0.9 governance routes are gone, not aliased: an admin calling one gets 404 or 405. The new
// routes answer in TestRenameMap.
func TestOldGovernanceRoutesAreGone(t *testing.T) {
	srv, _ := govCrudServer(t, &govCrudStore{})
	id := "aaaaaaaa-0000-4000-8000-000000000001"
	for _, c := range []struct{ method, old string }{
		{http.MethodGet, "/api/v1/governance/profiles"},
		{http.MethodPost, "/api/v1/governance/profiles"},
		{http.MethodPut, "/api/v1/governance/profiles/" + id},
		{http.MethodDelete, "/api/v1/governance/profiles/" + id},
		{http.MethodPost, "/api/v1/governance/assignments"},
		{http.MethodDelete, "/api/v1/governance/assignments/" + id},
		{http.MethodPost, "/api/v1/governance/preview"},
	} {
		w := do(t, srv, c.method, c.old, adminToken, `{}`)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404 or 405", c.method, c.old, w.Code)
		}
	}
}
