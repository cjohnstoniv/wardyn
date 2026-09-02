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

// TestPreCodecBumpCookieReDerives is the PF-26 codec-bump pin, and it REPLACES
// an earlier test that asserted the opposite (a pre-0.6 cookie keeps
// authenticating). That contract could not survive the truncation bit, and the
// reason is the whole argument for spending a forced re-login:
//
// GroupsTruncated has no tolerant decoding. An old payload carries no such key,
// so it reads as false — "this snapshot is complete" — which is the FAIL-OPEN
// answer. A human whose snapshot was already truncated at their last login
// would hold a cookie asserting completeness, and internal/api would hand them
// the deployment ceiling instead of the narrower governance profile their group
// is assigned: a silent widening, with no refusal and no audit line, for
// exactly the group-heavy directory the cap exists to serve. Nothing in the
// payload distinguishes "old cookie" from "new cookie, not truncated", so the
// payload gets a version and an old one is simply not a session.
//
// The cost is ONE extra login, once — the same re-login PF-12 already documents
// — and it is bounded: Middleware falls through with no principal, the browser
// is bounced to sign in, and CallbackHandler mints a current cookie.
//
// Counterfactual: drop the `sess.V != SessionCodecVersion` check in
// decodeSession and this test fails on the first leg while the pre-bump cookie
// silently decodes GroupsTruncated=false — the fail-open state the check exists
// to make unreachable.
func TestPreCodecBumpCookieReDerives(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	// Byte-for-byte a pre-0.7 payload: no "v" key, and no "groups" either (this
	// is what a 0.5 binary actually wrote).
	payload := []byte(fmt.Sprintf(`{"sub":"sub-legacy","email":"legacy@example.com","role":%q,"expiry":%q}`,
		writoidc.RoleMember, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)))
	if strings.Contains(string(payload), `"v"`) {
		t.Fatal("fixture is not a pre-codec-bump payload")
	}

	sub, _, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, payload))
	if ok || sub != "" {
		t.Fatalf("a pre-0.7 cookie authenticated (sub=%q) — its GroupsTruncated decodes as false, which is the fail-open reading", sub)
	}

	// A CURRENT-version payload that simply omits "groups" must still decode to
	// NIL: the bump versions the payload, it does not collapse the nil-vs-empty
	// distinction internal/api turns into groups_snapshot_stale.
	current := []byte(fmt.Sprintf(`{"v":%d,"sub":"sub-current","email":"c@example.com","role":%q,"expiry":%q}`,
		writoidc.SessionCodecVersion, writoidc.RoleMember,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)))
	sub, groups, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, current))
	if !ok || sub != "sub-current" {
		t.Fatalf("a current-version cookie did not authenticate (sub=%q)", sub)
	}
	if groups != nil {
		t.Errorf("groups = %v, want nil — an absent groups key must stay distinguishable from `asked, none`", groups)
	}
}

// TestTruncationBitSurvivesTheCookie is the session leg of PF-26 end to end:
// sessionGroups reports the drop, the bit encodes, and it comes back off the
// cookie as an authorization input rather than being recomputed (it CANNOT be
// recomputed — a truncated snapshot and a complete one are both just lists of
// plausible group names).
//
// Counterfactual: leave GroupsTruncated off Session (or off
// contextWithPrincipal) and the second leg reads false, which is the answer
// that silently sheds a group-assigned governance profile.
func TestTruncationBitSurvivesTheCookie(t *testing.T) {
	claim := make([]string, 400)
	for i := range claim {
		claim[i] = fmt.Sprintf("group-%04d-with-a-realistically-long-name", i)
	}
	if !writoidc.SessionGroupsTruncatedForTest(nil, claim, nil) {
		t.Fatal("400 long group names did not trip the byte cap; the fixture proves nothing")
	}
	if writoidc.SessionGroupsTruncatedForTest(nil, []string{"eng", "platform"}, nil) {
		t.Error("two short groups reported as truncated — the bit would fire on every ordinary login")
	}

	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	kept := writoidc.SessionGroupsForTest(nil, claim)
	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-many-groups", Email: "many@example.com", Role: writoidc.RoleMember,
		Expiry: time.Now().Add(time.Hour), Groups: kept, GroupsTruncated: true,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var truncated bool
	var authed bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		truncated = writoidc.GroupsTruncatedFromContext(r.Context())
		authed = writoidc.PrincipalFromContext(r.Context()) != ""
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	if !authed {
		t.Fatal("the truncated-snapshot session did not authenticate; truncation is a narrowing signal, not a rejection")
	}
	if !truncated {
		t.Error("GroupsTruncatedFromContext = false for a cookie minted with the bit set — the snapshot reads as complete")
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
