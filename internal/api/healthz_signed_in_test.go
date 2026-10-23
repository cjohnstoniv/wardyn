// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// healthzSubstrateRunner reports a runtime behind its one class, so the body has a substrate to withhold.
type healthzSubstrateRunner struct{ runner.Runner }

func (healthzSubstrateRunner) Name() string { return "docker" }
func (healthzSubstrateRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "docker", ConfinementClasses: []types.ConfinementClass{types.CC1},
		Resolved: map[types.ConfinementClass]string{types.CC1: "oci/runc"}}, nil
}

// The field census of GET /healthz: anonymous, it names the runner but neither the runtime behind each
// class nor the components, and asking writes no auth.fail row; signed in, by session or by admin
// token, it names all three. A credential the gate refuses gets the anonymous body, still 200.
func TestHealthz_SignedInFieldCensus(t *testing.T) {
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = healthzSubstrateRunner{}
	cfg.Components = map[string]ComponentInfo{"recording": {Selected: "pg"}}
	srv := New(cfg)
	session := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	tampered := *session
	tampered.Value += "x"
	// Signed validly but expired: it passes the presence check, so the gate itself refuses it and clears it.
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: "sub-member", Email: "member@corp.example",
		Role: oidc.RoleUser, UserType: types.UserTypeStandard, Expiry: time.Now().UTC().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	expired := signedSessionCookie(payload)
	for name, tc := range map[string]struct {
		bearer   string
		cookie   *http.Cookie
		signedIn bool
		cleared  bool // the gate's clearing of the cookie reaches the response
	}{
		"anonymous":                    {},
		"a session cookie that fails":  {cookie: &tampered},
		"an expired session":           {cookie: expired, cleared: true},
		"a wrong admin token":          {bearer: "not-the-token"},
		"signed in by session":         {cookie: session, signedIn: true},
		"signed in by the admin token": {bearer: adminToken, signedIn: true},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(h.audit.snapshot())
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.cookie != nil {
				r.AddCookie(tc.cookie)
			}
			w := httptest.NewRecorder()
			panicFails(t, srv.Handler()).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d %s, want 200", w.Code, w.Body)
			}
			if set := w.Result().Header.Values("Set-Cookie"); tc.cleared && (len(set) != 1 || !strings.Contains(set[0], "Max-Age=0")) {
				t.Errorf("Set-Cookie = %q, want the one session cookie cleared (Max-Age=0)", set)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if string(body["runner"]) != `"docker"` {
				t.Errorf("runner = %s, want \"docker\" for every caller", body["runner"])
			}
			for _, key := range []string{"confinement_substrates", "components"} {
				if _, has := body[key]; has != tc.signedIn {
					t.Errorf("%s present = %v, want %v; body %s", key, has, tc.signedIn, w.Body)
				}
			}
			if name == "anonymous" {
				for _, ev := range h.audit.snapshot()[before:] {
					if ev.Action == "auth.fail" {
						t.Errorf("an anonymous /healthz wrote auth.fail: %+v", ev)
					}
				}
			}
		})
	}
}
