// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F2-sso-to-ceiling PROBE 3 — destination: internal/auth/oidc/groups_overage_probe_test.go
//
// *** GREEN PIN. This started as the probe for hypothesis H1 of
// *** local/review-0.7/deep/F2-sso-to-ceiling.md, red on the RC; the fix landed
// *** and it is now a REGRESSION pin, unchanged. A failure here means the
// *** overage marker stopped reaching sessionGroups.
//
// INVARIANT UNDER TEST: a login whose ID token carries an IdP-side GROUPS
// OVERAGE marker instead of the groups themselves (Entra ID emits
// `_claim_names: {"groups": "src1"}` + `_claim_sources` and OMITS `groups`
// when the user is in more groups than the token limit — 150 for JWTs) must
// stamp GroupsTruncated=true, so internal/api's ceiling resolver treats the
// snapshot as unanswerable (effectiveCeiling in internal/api/governance.go)
// rather than as "asked, there were none" and hands the member the deployment
// ceiling.
//
// Today (CallbackHandler's groups-claim decode in oidc.go, then sessionGroups
// in derive.go) the absent `groups` decodes to nil, sessionGroups returns
// (empty non-nil, truncated=false), and the walled group has evaporated exactly
// the way PF-26 closed for the cookie byte cap.
//
// Run:
//
//	cd /home/cjohn/wt-v07-profiles && \
//	cp local/review-0.7/deep/F2-sso-to-ceiling/groups_overage_probe_test.go internal/auth/oidc/ && \
//	nice -n 10 GOMAXPROCS=8 go test ./internal/auth/oidc/ -run 'TestF2_GroupsOverage' -count=1 -p 4 -v ; \
//	rm -f internal/auth/oidc/groups_overage_probe_test.go
package oidc_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// f2BuildOverageIDToken signs the FULL Entra groups-overage shape: no "groups"
// claim at all, plus BOTH halves of the distributed-claim pointer.
//
// idpEnv.buildIDTokenRawClaim can carry only ONE extra claim, and a token with
// "_claim_names" but no matching "_claim_sources" entry is rejected outright by
// go-oidc's verifier ("oidc: source does not exist",
// IDTokenVerifier.Verify in go-oidc/v3@v3.20.0) — a 401 before any Wardyn code
// runs, which would make this probe fail for a reason that has nothing to do
// with the invariant. A real Entra overage token always sends both.
func f2BuildOverageIDToken(t *testing.T, e *idpEnv, sub, email string) string {
	t.Helper()
	claims := map[string]any{
		"iss": e.httpSrv.URL, "sub": sub, "aud": e.clientID,
		"email": email, "email_verified": true, "nonce": roleCallbackNonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		// "groups" deliberately ABSENT — that is the overage.
		"_claim_names": map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{
			"endpoint": "https://graph.microsoft.com/v1.0/users/" + sub + "/getMemberObjects",
		}},
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal overage claims: %v", err)
	}
	tok := oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
	e.latestIDTok = tok
	return tok
}

func TestF2_GroupsOverageMarkerStampsTruncated(t *testing.T) {
	env := newIdPEnv(t)
	// A member-role map so the login is admitted with RoleMember (an admin
	// would short-circuit the ceiling anyway and prove nothing).
	auth := env.newRoleAuth(t, map[string]string{"eng-team": "member"}, "member", nil)

	// Entra overage shape: NO "groups" key at all, a _claim_names/_claim_sources
	// pointer pair instead.
	f2BuildOverageIDToken(t, env, "sub-overage", "many@corp.example")
	w, sess := doCallback(t, auth)
	if w.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302 (the login itself must still succeed): %s", w.Code, w.Body.String())
	}
	if sess.Groups == nil {
		t.Fatalf("groups = nil; a fresh 0.7 login must never read as a pre-0.6 cookie")
	}
	// doCallback's Session omits the truncation bit, so read it off the minted
	// cookie through Middleware exactly as internal/api's humanOrAdminAuth does
	// (humanOrAdminAuth in http.go -> oidc.GroupsTruncatedFromContext).
	var sessCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no wardyn_session cookie minted")
	}
	var truncated bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		truncated = writoidc.GroupsTruncatedFromContext(r.Context())
	})
	checkReq := httptest.NewRequest(http.MethodGet, "/", nil)
	checkReq.AddCookie(sessCookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), checkReq)
	if !truncated {
		t.Errorf("GroupsTruncated = false for an overage login (groups omitted by the IdP, _claim_names present): " +
			"the snapshot reads as COMPLETE-AND-EMPTY, so a group-tier governance assignment that walls this member evaporates silently (H1)")
	}
}
