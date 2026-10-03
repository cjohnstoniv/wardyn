// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// fakeGate is an IdentityGate that records what it is asked and answers what the case says.
type fakeGate struct {
	refused   bool
	refuseErr error
	issueErr  error
	epoch     int64
	refs      []writoidc.IdentityRef
	issued    []writoidc.LoginFacts
}

func (g *fakeGate) Refused(_ context.Context, ref writoidc.IdentityRef) (bool, error) {
	g.refs = append(g.refs, ref)
	return g.refused, g.refuseErr
}

func (g *fakeGate) Issue(_ context.Context, f writoidc.LoginFacts) (int64, error) {
	g.issued = append(g.issued, f)
	return g.epoch, g.issueErr
}

// epochSink records the authority epoch each captured login grant was offered under.
type epochSink struct {
	epochs  []int64
	has     []bool
	subject []string
}

func (s *epochSink) LoginScopes(context.Context) []string { return nil }
func (s *epochSink) CaptureLoginGrant(ctx context.Context, subject string, _ writoidc.LoginGrant) {
	e, ok := writoidc.AuthorityEpochFromContext(ctx)
	s.epochs, s.has, s.subject = append(s.epochs, e), append(s.has, ok), append(s.subject, subject)
}

func sessionPayload(t *testing.T, w *httptest.ResponseRecorder) (writoidc.Session, map[string]any) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name != "wardyn_session" || c.Value == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(c.Value, ".", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		var s writoidc.Session
		var fields map[string]any
		if json.Unmarshal(raw, &s) != nil || json.Unmarshal(raw, &fields) != nil {
			t.Fatalf("session payload %s", raw)
		}
		return s, fields
	}
	t.Fatal("no session cookie issued")
	return writoidc.Session{}, nil
}

// Admission refuses a deactivated identity on every issuer, after the principal is resolved and
// before anything is derived or recorded; the browser sees only sign_in_refused, and the refusal is
// reported to onDenied as identity_deactivated. A gate that cannot answer fails the login closed.
func TestAdmitRefusesADeactivatedIdentity(t *testing.T) {
	for _, c := range []struct {
		name       string
		gate       *fakeGate
		wantError  string
		wantDenied []string
	}{
		{name: "deactivated", gate: &fakeGate{refused: true}, wantError: "sign_in_refused", wantDenied: []string{writoidc.DenialIdentityDeactivated}},
		{name: "gate unavailable", gate: &fakeGate{refuseErr: errors.New("pg down")}, wantError: "role_check_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			var logins []string
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.Identities = c.gate
				cfg.OnLogin = func(_ context.Context, f writoidc.LoginFacts) { logins = append(logins, f.Sub) }
			})
			env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
			var denied []string
			w, _ := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(nil, func(_ *http.Request, reason string) { denied = append(denied, reason) }))
			if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, c.wantError) {
				t.Errorf("Location = %q, want auth_error=%s", loc, c.wantError)
			}
			if sessionIssuedBy(w) {
				t.Error("a session was issued for a refused identity")
			}
			if len(c.gate.issued) != 0 || len(logins) != 0 {
				t.Errorf("a refused sign-in reached issuance %v or OnLogin %v", c.gate.issued, logins)
			}
			if !slices.Equal(denied, c.wantDenied) {
				t.Errorf("denials = %v, want %v", denied, c.wantDenied)
			}
			if len(c.gate.refs) != 1 || c.gate.refs[0].Principal != "pairwise-1" || c.gate.refs[0].Issuer != env.httpSrv.URL {
				t.Errorf("gate asked about %+v, want the resolved principal under the token's issuer", c.gate.refs)
			}
		})
	}
}

func sessionIssuedBy(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return true
		}
	}
	return false
}

// Only an Entra token that carries a tenant and a well-formed object id is asked about by them.
func TestAdmitAsksAboutTheEntraKeyOnlyOnEntra(t *testing.T) {
	for _, c := range []struct {
		name         string
		entra        bool
		oid          string
		wantT, wantO string
	}{
		{name: "entra", entra: true, oid: keyObject, wantT: keyTenant, wantO: keyObject},
		{name: "entra, malformed oid", entra: true, oid: "not-a-guid"},
		{name: "not entra", oid: keyObject},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			gate := &fakeGate{}
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Identities = gate })
			if c.entra {
				writoidc.SetEntraForTest(auth)
			}
			env.entraIDToken(t, "pairwise-1", keyTenant, c.oid)
			doCallbackVia(t, auth, auth.CallbackHandler)
			if len(gate.refs) != 1 || gate.refs[0].TenantID != c.wantT || gate.refs[0].ObjectID != c.wantO {
				t.Errorf("gate asked about %+v, want tenant %q object %q", gate.refs, c.wantT, c.wantO)
			}
		})
	}
}

// Issuance is the second gate: a refusal there (a suspension landed between admission and issuance), a
// binding conflict or an unreadable store each end the login with no session, no OnLogin and no
// captured credential.
func TestIssuanceRefusalsEndTheLogin(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		wantError  string
		wantDenied []string
	}{
		{name: "suspended in flight", err: writoidc.ErrIdentityDeactivated, wantError: "sign_in_refused", wantDenied: []string{writoidc.DenialIdentityDeactivated}},
		{name: "binding conflict", err: writoidc.ErrIdentityConflict, wantError: "sign_in_refused"},
		{name: "store down", err: errors.New("pg down"), wantError: "role_check_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			gate := &fakeGate{issueErr: c.err}
			var logins []string
			sink := &epochSink{}
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.Identities = gate
				cfg.OnLogin = func(_ context.Context, f writoidc.LoginFacts) { logins = append(logins, f.Sub) }
			})
			auth.AttachLoginGrantSink(sink)
			env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
			var denied []string
			w, _ := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(nil, func(_ *http.Request, reason string) { denied = append(denied, reason) }))
			if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, c.wantError) {
				t.Errorf("Location = %q, want auth_error=%s", loc, c.wantError)
			}
			if sessionIssuedBy(w) || len(logins) != 0 || len(sink.subject) != 0 {
				t.Errorf("a refused issuance left a session (%v), OnLogin (%v) or a capture (%v)", sessionIssuedBy(w), logins, sink.subject)
			}
			if !slices.Equal(denied, c.wantDenied) {
				t.Errorf("denials = %v, want %v", denied, c.wantDenied)
			}
		})
	}
}

// A login that clears both gates carries the authority epoch the gate returned on its cookie.
func TestIssuedEpochRidesTheCookie(t *testing.T) {
	env := newIdPEnv(t)
	gate := &fakeGate{epoch: 7}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Identities = gate })
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	sess, fields := sessionPayload(t, w)
	if sess.AuthorityEpoch != 7 || fields["ae"] != float64(7) {
		t.Errorf("cookie epoch = %d (ae=%v), want 7", sess.AuthorityEpoch, fields["ae"])
	}
	if len(gate.issued) != 1 || gate.issued[0].Sub != "pairwise-1" || gate.issued[0].Email != "pat@corp.example" {
		t.Errorf("issuance saw %+v, want the person who signed in", gate.issued)
	}
	if e, ok := writoidc.AuthorityEpochFromContext(writoidc.WithAuthorityEpoch(context.Background(), 7)); !ok || e != 7 {
		t.Errorf("WithAuthorityEpoch round trip = %d, %v", e, ok)
	}

	// With epoch 0 the field is omitted, so a cookie from before this release and one from an
	// identity never suspended read the same.
	gate.epoch = 0
	env.buildIDToken(t, "pairwise-2", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ = doCallbackVia(t, auth, auth.CallbackHandler)
	if _, fields = sessionPayload(t, w); fields["ae"] != nil {
		t.Errorf("epoch 0 was written to the cookie: %v", fields["ae"])
	}
}

type identityRevocations struct {
	status  writoidc.SessionStatus
	err     error
	epochs  []int64
	revoked bool
}

func (r *identityRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return r.revoked, r.err
}
func (r *identityRevocations) RevokeSub(context.Context, string) error { return nil }
func (r *identityRevocations) RevokeAll(context.Context) error         { return nil }
func (r *identityRevocations) SessionStatus(_ context.Context, _, _ string, _ time.Time, epoch int64) (writoidc.SessionStatus, error) {
	r.epochs = append(r.epochs, epoch)
	return r.status, r.err
}

// The cookie middleware asks the revocation store about the cookie's own epoch, and a deactivated
// answer clears the cookie and names the reason; a store error fails closed; a store that cannot read
// identity rows (a plain SessionRevocations) is the cutoff check alone, as before.
func TestMiddlewareChecksTheCookieEpoch(t *testing.T) {
	for _, c := range []struct {
		name       string
		rev        *identityRevocations
		wantReason string
		wantLive   bool
	}{
		{name: "live", rev: &identityRevocations{}, wantLive: true},
		{name: "deactivated or past epoch", rev: &identityRevocations{status: writoidc.SessionDeactivated}, wantReason: "identity_deactivated"},
		{name: "cutoff", rev: &identityRevocations{status: writoidc.SessionRevoked}, wantReason: "revoked_session"},
		{name: "store error", rev: &identityRevocations{err: errors.New("pg down")}, wantReason: "session_revocation_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			gate := &fakeGate{epoch: 4}
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.Identities = gate
				cfg.Revocations = c.rev
			})
			env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
			w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
			var cookie *http.Cookie
			for _, ck := range w.Result().Cookies() {
				if ck.Name == "wardyn_session" {
					cookie = ck
				}
			}
			if cookie == nil {
				t.Fatal("no session issued")
			}
			var live bool
			var reason string
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				live = writoidc.PrincipalFromContext(r.Context()) != ""
				reason = writoidc.SessionRejectedFromContext(r.Context())
			})).ServeHTTP(rec, req)
			if live != c.wantLive || reason != c.wantReason {
				t.Errorf("live=%v reason=%q, want live=%v reason=%q", live, reason, c.wantLive, c.wantReason)
			}
			if len(c.rev.epochs) == 0 || c.rev.epochs[len(c.rev.epochs)-1] != 4 {
				t.Errorf("the store was asked about epochs %v, want the cookie's 4", c.rev.epochs)
			}
		})
	}
}

// CheckSession on a store that cannot read identity rows is the cutoff check alone.
func TestCheckSessionFallsBackToTheCutoff(t *testing.T) {
	plain := &plainRevocations{revoked: true}
	if st, err := writoidc.CheckSession(context.Background(), plain, "sub", "", time.Now(), 3); err != nil || st != writoidc.SessionRevoked {
		t.Errorf("a cutoff = %v, %v; want SessionRevoked", st, err)
	}
	plain.revoked = false
	if st, err := writoidc.CheckSession(context.Background(), plain, "sub", "", time.Now(), 3); err != nil || st != writoidc.SessionLive {
		t.Errorf("no cutoff = %v, %v; want SessionLive", st, err)
	}
}

type plainRevocations struct{ revoked bool }

func (p *plainRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return p.revoked, nil
}
func (p *plainRevocations) RevokeSub(context.Context, string) error { return nil }
func (p *plainRevocations) RevokeAll(context.Context) error         { return nil }

// A portal's token exchange is admitted through the same gate: a deactivated identity is refused with
// identity_deactivated, which the exchange records.
func TestVerifySubjectTokenRefusesADeactivatedIdentity(t *testing.T) {
	env := newIdPEnv(t)
	gate := &fakeGate{refused: true}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Identities = gate })
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, env.subjectClaims()), testPortalClient, nil)
	if denied != writoidc.DenialIdentityDeactivated || sess.Sub != "" {
		t.Fatalf("exchange = %+v, %q; want refused as %s", sess, denied, writoidc.DenialIdentityDeactivated)
	}
	if len(gate.issued) != 0 {
		t.Errorf("an exchange issued an identity: %v", gate.issued)
	}
}
