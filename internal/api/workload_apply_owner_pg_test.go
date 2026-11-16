// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestPG_WorkloadApply_RefusesAnotherOwnersQuery: an apply, and its preview, are always the caller's own, so a
// ?owner= naming anyone else, an administrator's included, is a 400 and writes nothing; naming the caller is the
// apply.
func TestPG_WorkloadApply_RefusesAnotherOwnersQuery(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	doors := map[string]string{http.MethodPut: "/api/v1/workloads/web?owner=mem", http.MethodPost: "/api/v1/workloads/web/preview?owner=mem"}
	for who, c := range map[string]*http.Cookie{"another person": e.other, "an administrator": e.admin} {
		for method, path := range doors {
			w := doSSO(t, e.h.srv, method, path, c, workloadBody(pol, "t", ""))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonWorkloadInvalid) {
				t.Fatalf("%s: %s %s = %d %s, want 400 %s", who, method, path, w.Code, w.Body, reasonWorkloadInvalid)
			}
		}
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/workloads/web/preview?owner=mem", e.mem, workloadBody(pol, "t", "")); w.Code != http.StatusOK {
		t.Fatalf("mem's own preview = %d %s, want 200", w.Code, w.Body)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads/web?owner=mem", e.admin, ""); w.Code != http.StatusNotFound {
		t.Fatalf("mem's web after the refused applies = %d %s, want 404: nothing written", w.Code, w.Body)
	}
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web?owner=mem", e.mem, workloadBody(pol, "t", "")), http.StatusCreated)
}
