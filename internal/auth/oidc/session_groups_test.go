// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Session.Groups: the login-time group snapshot a `group`-subject capability
// grant matches against. Three properties are load-bearing and each has its own
// failure mode — normalization (a grant that silently never matches), the byte
// cap (a cookie the browser drops entirely, i.e. nobody can sign in), and
// nil-vs-empty (a pre-0.6 cookie mistaken for "this human has no groups").
package oidc_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func TestSessionGroupsNormalizes(t *testing.T) {
	// Both claims fold into one set. An Entra App Role ("Wardyn.Contractors")
	// and a group are indistinguishable afterwards, which is the point: an
	// admin grants against either without knowing which claim carried it.
	got := writoidc.SessionGroupsForTest(
		[]string{"Wardyn.Contractors", "  eng-team  ", "ENG-TEAM"},
		[]string{"eng-team", "Platform", ""},
	)
	want := []string{"eng-team", "platform", "wardyn.contractors"}
	if !slices.Equal(got, want) {
		t.Errorf("groups = %v, want %v (union, trimmed, lowercased, deduped, sorted)", got, want)
	}
}

// TestSessionGroupsDropsUnmatchableClaims: a grant subject is an
// operator-typed ASCII string, and Unicode case folding lets a crafted claim
// fold ONTO one (the same escalation asciiOnly guards in deriveRole). A claim
// that could never be written down as a grant subject is dropped rather than
// carried into the cookie where it can only ever surprise someone.
func TestSessionGroupsDropsUnmatchableClaims(t *testing.T) {
	got := writoidc.SessionGroupsForTest(nil, []string{
		"admın",      // dotless i — folds onto "admin" under Unicode rules
		"roſs",       // long s — folds onto "ross"
		"eng\ttab",   // control character
		"  ",         // whitespace only
		"real-group", // the only survivor
	})
	if !slices.Equal(got, []string{"real-group"}) {
		t.Errorf("groups = %v, want only [real-group] — a claim no grant can name must not be carried", got)
	}
}

// TestSessionGroupsNeverNil: nil is RESERVED for "this cookie predates 0.6".
// A 0.6 login whose IdP sent nothing must produce the empty NON-NIL slice, or
// every such human is permanently reported as having a stale group snapshot.
func TestSessionGroupsNeverNil(t *testing.T) {
	got := writoidc.SessionGroupsForTest(nil, nil)
	if got == nil {
		t.Fatal("sessionGroups returned nil for empty claims; nil means `pre-0.6 cookie`, not `no groups`")
	}
	if len(got) != 0 {
		t.Errorf("groups = %v, want empty", got)
	}
}

// TestSessionGroupsCapKeepsTheCookieUsable is the one that protects login
// itself: a browser drops a cookie over ~4096 bytes ENTIRELY — no error, no
// truncation, just a human who cannot stay signed in. A user carrying hundreds
// of AD groups is ordinary, so the cap is not a theoretical bound.
func TestSessionGroupsCapKeepsTheCookieUsable(t *testing.T) {
	claim := make([]string, 400)
	for i := range claim {
		claim[i] = fmt.Sprintf("group-%04d-with-a-realistically-long-name", i)
	}
	got := writoidc.SessionGroupsForTest(nil, claim)
	if len(got) == 0 || len(got) >= len(claim) {
		t.Fatalf("kept %d of %d groups, want a real truncation", len(got), len(claim))
	}

	// The budget is measured where it matters: the encoded cookie value.
	sess := writoidc.Session{
		Sub: "sub-with-a-long-opaque-idp-identifier-0123456789",
		// Roughly the longest email a real deployment produces.
		Email:  "firstname.lastname@a-fairly-long-corporate-domain.example.com",
		Role:   writoidc.RoleMember,
		Expiry: time.Now().Add(time.Hour),
		Groups: got,
	}
	env := newIdPEnv(t)
	cookie, err := writoidc.EncodeSessionForTest(env.newAuth(t, nil), sess)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n := len(cookie.Name) + len(cookie.Value); n > 4096 {
		t.Errorf("session cookie is %d bytes, over the ~4096 a browser will keep — login breaks silently", n)
	}

	// The truncation must be DETERMINISTIC, or an admin debugging "why does
	// this grant not apply" gets a different answer per login. Sorted order
	// makes the kept set a stable prefix.
	again := writoidc.SessionGroupsForTest(nil, claim)
	if !slices.Equal(got, again) {
		t.Error("two calls with identical claims kept different groups; the drop is not deterministic")
	}
	if !slices.IsSorted(got) {
		t.Error("kept groups are not sorted; the drop-from-end rule has no stable meaning")
	}

	// And the budget is actually the documented one, not accidentally larger.
	raw, _ := json.Marshal(got)
	if len(raw) > writoidc.MaxSessionGroupsBytesForTest+2 { // +2: the outer [] the per-entry cost does not charge for
		t.Errorf("groups payload is %d bytes, over the %d budget", len(raw), writoidc.MaxSessionGroupsBytesForTest)
	}
}

// ─── the cookie round trip ────────────────────────────────────────────────────

// sessionThroughMiddleware runs cookie back through Middleware and returns what
// the request context carried — the only view of a session the rest of the
// control plane ever gets.
func sessionThroughMiddleware(t *testing.T, auth *writoidc.Authenticator, cookie *http.Cookie) (sub string, groups []string, authenticated bool) {
	t.Helper()
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sub = writoidc.PrincipalFromContext(r.Context())
		groups = writoidc.GroupsFromContext(r.Context())
		authenticated = sub != ""
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	return sub, groups, authenticated
}

// TestPre06CookieStillAuthenticatesWithNilGroups is the upgrade-safety pin: a
// cookie written by a 0.5 binary has NO "groups" key at all. It must still
// authenticate — an upgrade that force-logs-out every signed-in human is not
// something this field gets to cause — and it must read back as NIL, the
// signal internal/api turns into groups_snapshot_stale rather than quietly
// deciding this person belongs to no groups.
func TestPre06CookieStillAuthenticatesWithNilGroups(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	// Byte-for-byte a pre-0.6 payload: four keys, no "groups".
	payload := []byte(fmt.Sprintf(`{"sub":"sub-legacy","email":"legacy@example.com","role":%q,"expiry":%q}`,
		writoidc.RoleMember, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)))
	if strings.Contains(string(payload), "groups") {
		t.Fatal("fixture is not a pre-0.6 payload")
	}

	sub, groups, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, payload))
	if !ok || sub != "sub-legacy" {
		t.Fatalf("pre-0.6 cookie did not authenticate (sub=%q); the upgrade forces a re-login", sub)
	}
	if groups != nil {
		t.Errorf("groups = %v, want nil — a pre-0.6 cookie must be distinguishable from `asked, none`", groups)
	}
}

// TestEmptyGroupsSurviveTheCookieAsNonNil is the other half: without it,
// `omitempty` (or any encoder that collapses [] to absent) would make a 0.6
// login with no groups indistinguishable from the legacy cookie above, and
// every such member would be told their snapshot is stale forever.
func TestEmptyGroupsSurviveTheCookieAsNonNil(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-empty", Email: "e@example.com", Role: writoidc.RoleMember,
		Expiry: time.Now().Add(time.Hour), Groups: []string{},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, groups, ok := sessionThroughMiddleware(t, auth, cookie)
	if !ok {
		t.Fatal("session did not authenticate")
	}
	if groups == nil {
		t.Fatal("an explicitly-empty group set decoded as nil; it is now indistinguishable from a pre-0.6 cookie")
	}
	if len(groups) != 0 {
		t.Errorf("groups = %v, want empty", groups)
	}
}

// ─── the callback stamp ───────────────────────────────────────────────────────

// TestCallbackStampsSessionGroups: the snapshot is taken at login, from the
// same two claims deriveRole consumes, and reaches the request context intact.
func TestCallbackStampsSessionGroups(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", nil)

	w, sess := doRoleCallback(t, env, auth, "bob@corp.example",
		[]string{"Wardyn.Contractors"}, []string{"eng-team", "ENG-TEAM"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	want := []string{"eng-team", "wardyn.contractors"}
	if !slices.Equal(sess.Groups, want) {
		t.Errorf("session groups = %v, want %v — App Roles and groups fold into one grantable set", sess.Groups, want)
	}
}

// TestCallbackMalformedGroupsClaimStampsEmptyNotNil: a real IdP sometimes
// sends "groups" as a bare scalar. That already must not fail the login (it
// decodes to nil and contributes nothing to the role — see the tolerant-decode
// tests); it must equally not produce a NIL snapshot, which would falsely
// report a fresh 0.6 session as holding a pre-upgrade cookie.
func TestCallbackMalformedGroupsClaimStampsEmptyNotNil(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, nil, "", nil)

	env.buildIDTokenRawClaim(t, "sub-scalar", "dana@corp.example", "groups", "eng-team")
	w, sess := doCallback(t, auth)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Groups == nil {
		t.Error("a malformed groups claim produced a nil snapshot; the session would read as pre-0.6 forever")
	}
	if len(sess.Groups) != 0 {
		t.Errorf("groups = %v, want empty (a scalar claim contributes nothing)", sess.Groups)
	}
}
