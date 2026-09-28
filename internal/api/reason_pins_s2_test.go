// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
)

// TestSlice2_QueryAndBodyReasons (#656 slice 2) pins the machine-readable
// `reason` on a representative set of setup/site-config/governance/
// people-access refusals that need no store or fixture beyond the bare
// harness — cheap, direct proof the wiring lands on the wire, mirroring
// slice 1's TestListApprovals_QueryParamReasons/TestDecodeAndValidateCreateRun_Reasons.
func TestSlice2_QueryAndBodyReasons(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		body        string
		wantStatus  int
		wantReason  string
		withSecrets bool // mountSecretRoutes only mounts /secrets/{name} when cfg.Secrets != nil
	}{
		{name: "GET /access with no OIDC configured", method: http.MethodGet, path: "/api/v1/access",
			wantStatus: http.StatusServiceUnavailable, wantReason: reasonSSONotConfigured},
		{name: "POST /admin/delegates with no delegate store", method: http.MethodPost, path: "/api/v1/admin/delegates",
			body: `{"name":"n","idp_client_id":"c","group":"g"}`, wantStatus: http.StatusNotImplemented, wantReason: reasonDelegationStoreUnavailable},
		{name: "POST /setup/onboarding-complete with no store", method: http.MethodPost, path: "/api/v1/setup/onboarding-complete",
			wantStatus: http.StatusServiceUnavailable, wantReason: reasonSetupOnboardingStoreUnavailable},
		{name: "PUT /secrets/{name} with an invalid name", method: http.MethodPut, path: "/api/v1/secrets/Not_Valid!",
			body: `{"value":"sk-test-a-long-enough-value"}`, wantStatus: http.StatusBadRequest, wantReason: reasonSecretNameInvalid, withSecrets: true},
		{name: "PUT /secrets/{name} with a reserved name", method: http.MethodPut, path: "/api/v1/secrets/wardyn-provider-x",
			body: `{"value":"sk-test-a-long-enough-value"}`, wantStatus: http.StatusForbidden, wantReason: reasonSecretNameReserved, withSecrets: true},
		{name: "DELETE /people/{principal}/ssh-keys with an empty principal", method: http.MethodDelete,
			path: "/api/v1/people/%20/ssh-keys", wantStatus: http.StatusBadRequest, wantReason: reasonSSHKeyAdminPrincipalRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			srv := h.srv
			if tc.withSecrets {
				cfg := baseTestConfig(h, nil)
				cfg.Secrets = &memSecrets{}
				srv = New(cfg)
			}
			w := do(t, srv, tc.method, tc.path, adminToken, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := errorReason(w); got != tc.wantReason {
				t.Errorf("reason = %q, want %q; body=%s", got, tc.wantReason, w.Body.String())
			}
		})
	}
}
