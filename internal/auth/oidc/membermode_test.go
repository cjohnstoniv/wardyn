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
		Expiry:          time.Now().UTC().Add(time.Hour).Truncate(time.Second),
		IssuedAt:        time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second),
		Groups:          []string{"wardyn.admin", "eng"},
		GroupsTruncated: true,
	}
}

// setMemberMode drives (*Authenticator).SetMemberMode over a request carrying
// `in` and returns the cookie it wrote.
func setMemberMode(t *testing.T, a *writoidc.Authenticator, in *http.Cookie, on bool) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/member-mode", nil)
	r.AddCookie(in)
	w := httptest.NewRecorder()
	if err := a.SetMemberMode(w, r, on); err != nil {
		t.Fatalf("SetMemberMode(%v): %v", on, err)
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
	if role != writoidc.RoleMember {
		t.Errorf("RoleFromContext = %q, want %q", role, writoidc.RoleMember)
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

// TestMemberMode_PreFlagCookieDecodes: a codec-v1 cookie written before the
// field existed carries no "mm" key at all and must still be a session, with
// the flag reading false. This is why the field is omitempty and the codec
// version is NOT bumped (a bump would 401 every live cookie mid-rollout).
func TestMemberMode_PreFlagCookieDecodes(t *testing.T) {
	a := &writoidc.Authenticator{}
	// Hand-rolled payload with NO mm key — the byte shape a 0.7.3 binary wrote.
	payload := []byte(`{"v":1,"sub":"sub-old","email":"old@corp.example","role":"admin",` +
		`"expiry":"` + time.Now().UTC().Add(time.Hour).Format(time.RFC3339) + `","groups":[]}`)
	c := writoidc.EncodeRawSessionForTest(a, payload)

	sub, role, mm, authed := principalOf(t, a, c)
	if !authed {
		t.Fatal("a pre-flag codec-v1 cookie must still authenticate — no codec bump")
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
