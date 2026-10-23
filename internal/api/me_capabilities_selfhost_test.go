// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeCapabilities_MaySelfHostRunner pins the may_self_host_runner bool the console reads to say "Personal
// runners aren't turned on for you" before a person copies commands that can only end in the mint refusal. It
// answers exactly as POST /me/runners/tokens does: self_host_runner is restricted (migration 0178), so nobody
// holds it until an allow names it; a deny row takes it back; an operator is exempt.
func TestMeCapabilities_MaySelfHostRunner(t *testing.T) {
	restricted := map[string]map[string]bool{capFeature: {featureSelfHostRunner: true}}
	allow := grant(types.CapabilitySubjectUser, capEmail, capFeature, featureSelfHostRunner, types.CapabilityAllow)
	deny := grant(types.CapabilitySubjectUser, capEmail, capFeature, featureSelfHostRunner, types.CapabilityDeny)
	for name, tc := range map[string]struct {
		grants []types.CapabilityGrant
		role   string
		want   bool
	}{
		"default: restricted and no allow": {nil, oidc.RoleUser, false},
		"granted":                          {[]types.CapabilityGrant{allow}, oidc.RoleUser, true},
		"removed: a deny beats the allow":  {[]types.CapabilityGrant{allow, deny}, oidc.RoleUser, false},
		"operator is exempt":               {nil, oidc.RoleAdmin, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.OIDC = &oidc.Authenticator{}
			h.srv.cfg.Store = &capStore{grants: tc.grants, enf: map[string]bool{}, restricted: restricted}
			h.srv.router = h.srv.routes()
			w := doSSO(t, h.srv, http.MethodGet, "/api/v1/me/capabilities", ssoSession(t, capSub, capEmail, tc.role), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /me/capabilities = %d: %s", w.Code, w.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := string(raw["may_self_host_runner"]); got != map[bool]string{true: "true", false: "false"}[tc.want] {
				t.Fatalf("may_self_host_runner = %q, want %v (body %s)", got, tc.want, w.Body.String())
			}
		})
	}
}
