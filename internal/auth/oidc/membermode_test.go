// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

// The four invariants that keep "view as member" from becoming a privilege
// primitive (P2, v0.7.4). Driven through the REAL cookie round trip — encode,
// SetMemberMode, Middleware — rather than against the struct, because the whole
// design rests on what survives a re-encode: the clamp is applied at
// contextWithPrincipal and the stamped Role is never rewritten, so a test that
// only read the Session back would prove none of it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// memberModeSession is the admin session every case here starts from: every
// field the toggle must carry across byte-identical, populated with a value
// that is distinguishable from its zero.
func memberModeSession() writoidc.Session {
	return writoidc.Session{
		Sub:             "sub-admin-1",
		Email:           "admin@corp.example",
		Name:            "Ada Admin",
		Role:            writoidc.RoleAdmin,
		UserType:        "standard",
		Expiry:          time.Now().UTC().Add(time.Hour).Truncate(time.Second),
		IssuedAt:        time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second),
		Groups:          []string{"wardyn.admin", "eng"},
		GroupsTruncated: true,
	}
}

// setMemberMode drives (*Authenticator).SetMemberMode over a request carrying
// `in` and returns the cookie it wrote. noCredential is variadic so every case
// written before the 0.7.5 posture existed still reads as "the plain mode".
func setMemberMode(t *testing.T, a *writoidc.Authenticator, in *http.Cookie, on bool, noCredential ...bool) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/member-mode", nil)
	r.AddCookie(in)
	w := httptest.NewRecorder()
	stamped, err := a.SetMemberMode(w, r, on, len(noCredential) > 0 && noCredential[0])
	if err != nil {
		t.Fatalf("SetMemberMode(%v): %v", on, err)
	}
	if stamped != writoidc.RoleAdmin {
		t.Errorf("SetMemberMode returned stamped role %q, want %q — the audit row records what is PAUSED", stamped, writoidc.RoleAdmin)
	}
	got := w.Result().Cookies()
	if len(got) != 1 {
		t.Fatalf("SetMemberMode wrote %d cookies, want exactly 1", len(got))
	}
	return got[0]
}

// principalOf drives a cookie through Middleware and reports what the context
// published — the ONLY reading of the session any authorization decision makes.
func principalOf(t *testing.T, a *writoidc.Authenticator, c *http.Cookie) (sub, role string, memberMode, authed bool) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub = writoidc.PrincipalFromContext(r.Context())
		role = writoidc.RoleFromContext(r.Context())
		memberMode = writoidc.MemberModeFromContext(r.Context())
		authed = sub != ""
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	a.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	return sub, role, memberMode, authed
}

// decodePayload reads the signed payload back out of a cookie value without
// verifying it — the test needs the STAMPED fields, which is exactly what
// decodeSession hides behind the clamp.
func decodePayload(t *testing.T, c *http.Cookie) writoidc.Session {
	t.Helper()
	var sess writoidc.Session
	encoded, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		t.Fatalf("cookie value %q has no payload/signature split", c.Value)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(payload, &sess); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return sess
}

// TestMemberMode_ClampsEffectiveRoleOnly: the toggle lowers the EFFECTIVE role
// and rewrites nothing else. The stamped Role stays admin in the cookie (it is
// the sign-in-derived truth and the only thing toggle-off can restore from),
// while RoleFromContext answers member.
func TestMemberMode_ClampsEffectiveRoleOnly(t *testing.T) {
	a := &writoidc.Authenticator{}
	before := memberModeSession()
	in, err := writoidc.EncodeSessionForTest(a, before)
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}

	out := setMemberMode(t, a, in, true)
	stamped := decodePayload(t, out)

	if stamped.Role != writoidc.RoleAdmin {
		t.Errorf("stamped Role = %q, want %q — the cookie's role is never rewritten", stamped.Role, writoidc.RoleAdmin)
	}
	if !stamped.MemberMode {
		t.Error("stamped MemberMode = false, want true")
	}
	if stamped.Sub != before.Sub || stamped.Email != before.Email || stamped.Name != before.Name {
		t.Errorf("identity drifted: sub/email/name = %q/%q/%q, want %q/%q/%q",
			stamped.Sub, stamped.Email, stamped.Name, before.Sub, before.Email, before.Name)
	}
	if !stamped.IssuedAt.Equal(before.IssuedAt) {
		t.Errorf("IssuedAt = %v, want %v — a toggle must never escape a RevokeSub cutoff", stamped.IssuedAt, before.IssuedAt)
	}
	if !stamped.Expiry.Equal(before.Expiry) {
		t.Errorf("Expiry = %v, want %v — a toggle must not extend the session", stamped.Expiry, before.Expiry)
	}
	if !stamped.GroupsTruncated {
		t.Error("GroupsTruncated = false, want true — the PF-26 bit must survive the re-encode")
	}
	if len(stamped.Groups) != len(before.Groups) {
		t.Fatalf("Groups = %v, want %v", stamped.Groups, before.Groups)
	}
	for i := range before.Groups {
		if stamped.Groups[i] != before.Groups[i] {
			t.Fatalf("Groups = %v, want %v", stamped.Groups, before.Groups)
		}
	}

	sub, role, mm, authed := principalOf(t, a, out)
	if !authed {
		t.Fatal("the re-encoded cookie no longer authenticates")
	}
	if sub != before.Sub {
		t.Errorf("PrincipalFromContext = %q, want %q — the actor stays the admin's own sub", sub, before.Sub)
	}
	if role != writoidc.RoleUser {
		t.Errorf("RoleFromContext = %q, want %q", role, writoidc.RoleUser)
	}
	if !mm {
		t.Error("MemberModeFromContext = false, want true")
	}
}

// TestMemberMode_OffRestoresTheStampedRole_NeverReDerives: toggling off reads
// the role back off the cookie. Nothing re-derives it from the Groups
// snapshot — which is truncated here on purpose: a re-derivation would answer
// member and strand the admin.
func TestMemberMode_OffRestoresTheStampedRole_NeverReDerives(t *testing.T) {
	a := &writoidc.Authenticator{}
	in, err := writoidc.EncodeSessionForTest(a, memberModeSession())
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}

	on := setMemberMode(t, a, in, true)
	off := setMemberMode(t, a, on, false)

	if stamped := decodePayload(t, off); stamped.MemberMode {
		t.Error("stamped MemberMode = true after toggling off")
	}
	sub, role, mm, authed := principalOf(t, a, off)
	if !authed {
		t.Fatal("the toggled-off cookie no longer authenticates")
	}
	if sub != "sub-admin-1" {
		t.Errorf("PrincipalFromContext = %q, want sub-admin-1", sub)
	}
	if role != writoidc.RoleAdmin {
		t.Errorf("RoleFromContext = %q, want %q — the stamped role is restored, never re-derived", role, writoidc.RoleAdmin)
	}
	if mm {
		t.Error("MemberModeFromContext = true after toggling off")
	}
}

// TestMemberMode_PreFlagCookieDecodes: a cookie carrying no "mm" key at all
// must still be a session, with the flag reading false. This is why the field
// is omitempty and the mode never bumped the codec version.
func TestMemberMode_PreFlagCookieDecodes(t *testing.T) {
	a := &writoidc.Authenticator{}
	// Hand-rolled payload with NO mm key.
	payload := []byte(`{"v":` + strconv.Itoa(writoidc.SessionCodecVersion) +
		`,"sub":"sub-old","email":"old@corp.example","role":"admin","ut":"standard",` +
		`"expiry":"` + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + `","groups":[]}`)
	c := writoidc.EncodeRawSessionForTest(a, payload)

	sub, role, mm, authed := principalOf(t, a, c)
	if !authed {
		t.Fatal("a cookie with no mm key must still authenticate")
	}
	if sub != "sub-old" || role != writoidc.RoleAdmin {
		t.Errorf("sub/role = %q/%q, want sub-old/admin", sub, role)
	}
	if mm {
		t.Error("MemberModeFromContext = true for a cookie with no mm key")
	}
}

// TestMemberMode_RevokedSessionStaysRevoked: IssuedAt is frozen across the
// toggle, so a session cut off by RevokeSub cannot toggle its way back to life.
func TestMemberMode_RevokedSessionStaysRevoked(t *testing.T) {
	a := &writoidc.Authenticator{}
	in, err := writoidc.EncodeSessionForTest(a, memberModeSession())
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}
	out := setMemberMode(t, a, in, true)

	writoidc.SetRevocationsForTest(a, &memberModeRevocations{revoked: true})
	if _, _, _, authed := principalOf(t, a, out); authed {
		t.Error("a revoked session authenticated after toggling member mode on")
	}
}

// memberModeRevocations is a local SessionRevocations that answers a fixed
// verdict — the fakeSessionRevocations in oidc_test.go counts calls this file
// does not need, and a second copy here keeps the two tests independent.
type memberModeRevocations struct{ revoked bool }

func (f *memberModeRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return f.revoked, nil
}
func (f *memberModeRevocations) RevokeSub(context.Context, string) error { return nil }
func (f *memberModeRevocations) RevokeAll(context.Context) error         { return nil }

// TestMemberMode_RealMemberTurningItOnWritesNoCookie is W6-4. The handler's own
// comment calls a real member toggling ON "a no-op 200 … they are already what
// they asked to be". It was not a no-op: it stamped mm:1 onto the member's
// cookie, after which /me answers member_mode:true, the console paints a banner
// naming an admin role they do not hold, and BOTH mint doors — which key on
// MemberModeFromContext, not on the stamped tier — 409 their own SSH key and
// API token. The member Getting Started's "Connect your tools · Add SSH key"
// card and docs/MEMBERS.md's SSH path both break until they find the Exit.
//
// No cookie at all, rather than a cookie with the flag cleared: re-signing a
// member's session to record a decision not to change it is a Set-Cookie
// nobody asked for on a request that changed nothing.
func TestMemberMode_RealMemberTurningItOnWritesNoCookie(t *testing.T) {
	a := &writoidc.Authenticator{}
	sess := memberModeSession()
	sess.Sub, sess.Role = "sub-real-member", writoidc.RoleUser
	in, err := writoidc.EncodeSessionForTest(a, sess)
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/member-mode", nil)
	r.AddCookie(in)
	w := httptest.NewRecorder()
	stamped, err := a.SetMemberMode(w, r, true, false)
	if err != nil {
		t.Fatalf("SetMemberMode: %v", err)
	}
	if stamped != writoidc.RoleUser {
		t.Errorf("stamped role = %q, want %q — the return value is the role the caller HOLDS", stamped, writoidc.RoleUser)
	}
	if got := w.Result().Cookies(); len(got) != 0 {
		t.Fatalf("SetMemberMode wrote %d cookies for a member turning the mode ON, want 0: %+v", len(got), got)
	}
	// The session it was handed is untouched, so the mint doors stay open.
	if _, _, mm, authed := principalOf(t, a, in); !authed || mm {
		t.Errorf("after the no-op: authed=%v member_mode=%v, want true/false", authed, mm)
	}

	// THE CONTROL: a member turning it OFF is the ordinary exit and still
	// re-signs — that direction has to work from inside the mode, and the
	// route is classMember precisely so it always does.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/api/v1/me/member-mode", nil)
	r.AddCookie(in)
	if _, err := a.SetMemberMode(w, r, false, false); err != nil {
		t.Fatalf("SetMemberMode(false): %v", err)
	}
	if got := w.Result().Cookies(); len(got) != 1 {
		t.Fatalf("turning it OFF wrote %d cookies, want 1", len(got))
	}
}

// previewOf drives a cookie through Middleware and reports the NO-CREDENTIAL
// posture the context published (MemberPreviewNoCredential) beside the plain
// mode bit — the only reading of the pair anything downstream makes.
func previewOf(t *testing.T, a *writoidc.Authenticator, c *http.Cookie) (memberMode, preview bool) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		memberMode = writoidc.MemberModeFromContext(r.Context())
		preview = writoidc.MemberPreviewNoCredential(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	a.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	return memberMode, preview
}

// TestMemberMode_NoCredentialNeverSurvivesExit: the preview posture (0.7.5,
// finding 3) is stored as `on && noCredential`, so turning the mode OFF clears
// it BY CONSTRUCTION rather than by a second statement somebody could delete.
// An admin who exits and re-enters the plain mode must read their own
// credential again — the alternative is an admin silently stuck with model
// access hidden and no control that says so.
//
// THE RE-ISSUE ARM. The one thing that could resurrect a cleared bit is a path
// that decodes a whole Session and re-signs it. There are exactly two
// encodeSession callers in this package — SetMemberMode (driven below in both
// directions) and the OIDC callback, which builds a FRESH Session literal from
// the id_token and therefore cannot carry a stale preview bit across a re-login.
// There is no sliding-window/renewal re-issue at all: Expiry and IssuedAt are
// copied verbatim, never extended. The scan below is what keeps that true — a
// future re-issue path lands here and has to decide, rather than inheriting the
// bit silently.
func TestMemberMode_NoCredentialNeverSurvivesExit(t *testing.T) {
	a := &writoidc.Authenticator{}
	in, err := writoidc.EncodeSessionForTest(a, memberModeSession())
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}

	on := setMemberMode(t, a, in, true, true)
	if stamped := decodePayload(t, on); !stamped.MemberMode || !stamped.MemberModeNoCredential {
		t.Fatalf("entering the preview stamped mm=%v mmnc=%v, want true/true", stamped.MemberMode, stamped.MemberModeNoCredential)
	}
	if mm, preview := previewOf(t, a, on); !mm || !preview {
		t.Fatalf("inside the preview: member_mode=%v preview=%v, want true/true", mm, preview)
	}

	// enabled:false WITH no_credential:true — the body a console bug, a stale
	// tab or a hand-rolled curl can send. `on && noCredential` is what makes it
	// an exit rather than a session that is out of the mode and still hiding its
	// own credential, with no banner left on screen to say so.
	off := setMemberMode(t, a, on, false, true)
	if stamped := decodePayload(t, off); stamped.MemberMode || stamped.MemberModeNoCredential {
		t.Fatalf("after enabled:false the cookie still carries mm=%v mmnc=%v", stamped.MemberMode, stamped.MemberModeNoCredential)
	}
	if mm, preview := previewOf(t, a, off); mm || preview {
		t.Fatalf("after the exit: member_mode=%v preview=%v, want false/false", mm, preview)
	}

	// Re-entering the PLAIN mode must not resurrect the posture: the admin asked
	// to view as a member, not as one who cannot see their own credential.
	again := setMemberMode(t, a, off, true)
	if stamped := decodePayload(t, again); !stamped.MemberMode || stamped.MemberModeNoCredential {
		t.Fatalf("re-entering the plain mode stamped mm=%v mmnc=%v, want true/false", stamped.MemberMode, stamped.MemberModeNoCredential)
	}
	if _, preview := previewOf(t, a, again); preview {
		t.Error("the plain mode published the no-credential posture")
	}

	// THE SCAN: exactly two re-sign sites, both accounted for above.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var callers, signers []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(e.Name())
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		// ".encodeSession(" and not "a.encodeSession(": a re-issue written on a
		// differently-named receiver would slip past the literal form.
		if e.Name() != "session_codec.go" && strings.Contains(string(src), ".encodeSession(") {
			callers = append(callers, e.Name())
		}
		// The second hole: a re-issue that signs a payload itself, bypassing
		// encodeSession entirely. sessionHMAC is the only way to do that, and it
		// must stay inside the codec.
		if strings.Contains(string(src), "sessionHMAC(") {
			signers = append(signers, e.Name())
		}
	}
	sort.Strings(callers)
	sort.Strings(signers)
	want := []string{"membermode.go", "oidc_callback.go"}
	if !slices.Equal(callers, want) {
		t.Errorf("session re-sign sites = %v, want %v — a NEW one must decide what it does with "+
			"MemberModeNoCredential (a re-issue that copies a whole decoded Session would carry the "+
			"preview across an exit); add it here once it has", callers, want)
	}
	if !slices.Equal(signers, []string{"session_codec.go"}) {
		t.Errorf("sessionHMAC call sites = %v, want [session_codec.go] — a cookie signed outside the "+
			"codec bypasses encodeSession and the scan above with it", signers)
	}
}

// TestMemberMode_NoCredentialWithoutTheModeIsInert: a hand-built cookie
// carrying "mmnc" with no "mm" publishes NOTHING. The posture is not a
// primitive of its own — contextWithPrincipal ANDs it with the mode — so
// there is no cookie shape that hides a credential from a session that is not
// in member mode at all.
func TestMemberMode_NoCredentialWithoutTheModeIsInert(t *testing.T) {
	a := &writoidc.Authenticator{}
	payload := []byte(`{"v":` + strconv.Itoa(writoidc.SessionCodecVersion) +
		`,"sub":"sub-admin-1","email":"admin@corp.example","role":"admin","ut":"standard","mmnc":true,` +
		`"expiry":"` + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + `","groups":[]}`)
	c := writoidc.EncodeRawSessionForTest(a, payload)

	sub, role, mm, authed := principalOf(t, a, c)
	if !authed || sub != "sub-admin-1" || role != writoidc.RoleAdmin {
		t.Fatalf("authed=%v sub=%q role=%q, want true/sub-admin-1/admin — an unknown-to-0.7.4 key must not break the session", authed, sub, role)
	}
	if mm {
		t.Error("MemberModeFromContext = true for a cookie with no mm key")
	}
	if _, preview := previewOf(t, a, c); preview {
		t.Error("MemberPreviewNoCredential = true for a cookie carrying mmnc with no mm — the posture is not a primitive of its own")
	}
}
