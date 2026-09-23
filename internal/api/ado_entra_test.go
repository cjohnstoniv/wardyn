// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// adoTestNow is the fixed clock every case in this file reads, so a stored
// expiry is an assertable value rather than "about now".
var adoTestNow = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

// adoCallbackURL is the console's own callback. Nothing ever dials it — the
// tests drive the callback handler directly — but it must be the URI the fake
// tenant has registered, because the fake refuses any other.
const adoCallbackURL = "https://console.example.invalid/api/v1/scm/azure-devops/callback"

// adoFixture is one wired-up sign-in: a fake Entra tenant, a server whose
// Azure DevOps configuration points at it through the test authority hatch, and
// an audit sink.
type adoFixture struct {
	srv   *Server
	audit *memAudit
	fake  *entrafake.Server
	cfg   ADOEntraConfig
}

func newADOFixture(t *testing.T) *adoFixture {
	t.Helper()
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	fake.SetRedirectURI(adoCallbackURL)

	f := &adoFixture{fake: fake, audit: &memAudit{}}
	f.cfg = ADOEntraConfig{
		RowID:       "ado-row-1",
		TenantID:    fake.TenantID(),
		ClientID:    fake.ClientID(),
		RedirectURL: adoCallbackURL,
		Scopes:      fake.ConsentedScopes()[1:], // drop the /.default entry: this lane refuses it
		// The 0.7.10 boundary holds: the row names the console's own sign-in
		// application, in the sign-in tenant.
		LoginClientID:      fake.ClientID(),
		LoginTenantID:      fake.TenantID(),
		AuthorityOverride:  fake.URL(),
		AllowTestEndpoints: true,
	}
	f.srv = &Server{cfg: Config{
		Secrets:      &memSecrets{m: map[string][]byte{}},
		MaskRegistry: secretmask.NewRegistry(),
		Now:          func() time.Time { return adoTestNow },
		Audit:        f.audit,
		ADOEntra:     func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil },
	}}
	return f
}

// signIn drives the sign-in door and returns the authorization URL it
// redirected to plus the one-time cookies it set.
func (f *adoFixture) signIn(t *testing.T, subject, query string) (string, []*http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin"+query, nil)
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	f.srv.handleADOSignIn(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("sign-in: status %d body %q; want a 302 to the authority", w.Code, w.Body.String())
	}
	return w.Header().Get("Location"), w.Result().Cookies()
}

// follow walks the authorization URL with a client that follows nothing, and
// returns the query the tenant redirected back with.
func follow(t *testing.T, authURL string) url.Values {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, authURL, nil)
	if err != nil {
		t.Fatalf("build authorize request: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query()
}

// callback drives the callback door with the given query and cookies.
func (f *adoFixture) callback(t *testing.T, subject string, q url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/callback?"+q.Encode(), nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	f.srv.handleADOCallback(w, r)
	return w
}

// capture runs a whole successful sign-in for subject and returns the
// callback's response.
func (f *adoFixture) capture(t *testing.T, subject string) *httptest.ResponseRecorder {
	t.Helper()
	authURL, cookies := f.signIn(t, subject, "")
	q := follow(t, authURL)
	if q.Get("code") == "" {
		t.Fatalf("the authority issued no code: %v", q)
	}
	return f.callback(t, subject, q, cookies)
}

// stored reads the blob back through the production read path.
func (f *adoFixture) stored(t *testing.T, owner string) (adoEntraBlob, bool) {
	t.Helper()
	blob, found, err := f.srv.readADOEntraBlob(context.Background(), owner, f.cfg.RowID)
	if err != nil {
		t.Fatalf("read the stored sign-in: %v", err)
	}
	return blob, found
}

// TestADOSignIn_AuthorizationRequestShape pins what the sign-in leg actually
// asks for: an authorization code with an S256 challenge, the row's ceiling
// plus offline access and openid, and a state the callback can check.
func TestADOSignIn_AuthorizationRequestShape(t *testing.T) {
	f := newADOFixture(t)
	authURL, cookies := f.signIn(t, f.fake.Subject(), "")

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q; want code", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q; want S256", got)
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Errorf("the authorization request is missing a challenge, a state or a nonce: %v", q)
	}
	asked := strings.Fields(q.Get("scope"))
	for _, want := range append(slices.Clone(f.cfg.Scopes), entraOfflineAccessScope, entraOpenIDScope) {
		if !slices.Contains(asked, want) {
			t.Errorf("the authorization request does not ask for %q (asked %v)", want, asked)
		}
	}
	// The challenge on the wire must be the hash of the verifier the cookie
	// holds — otherwise the callback's exchange could never succeed, and PKCE
	// would be decoration.
	var verifier, state string
	for _, c := range cookies {
		switch c.Name {
		case adoPKCECookieName:
			verifier = c.Value
		case adoStateCookieName:
			state = c.Value
		}
	}
	if verifier == "" || state == "" {
		t.Fatalf("the sign-in leg set no verifier/state cookie: %v", cookies)
	}
	if got, want := q.Get("code_challenge"), entrafake.Challenge(verifier); got != want {
		t.Errorf("code_challenge = %q; want the S256 transform of the verifier cookie", got)
	}
	if q.Get("state") != state {
		t.Error("the state on the wire is not the state in the cookie, so the callback could never bind it")
	}
	// The verifier is a one-time secret and must never be readable from script.
	for _, c := range cookies {
		if !c.HttpOnly {
			t.Errorf("cookie %q is not HttpOnly", c.Name)
		}
	}
}

// TestADOSignIn_NarrowsToARequestedSubset: the optional query asks consent for
// one capability instead of the whole ceiling, and anything outside the ceiling
// is refused rather than trimmed.
func TestADOSignIn_NarrowsToARequestedSubset(t *testing.T) {
	f := newADOFixture(t)
	want := f.cfg.Scopes[0]

	for _, param := range []string{"scopes", "capabilities"} {
		authURL, _ := f.signIn(t, f.fake.Subject(), "?"+param+"="+url.QueryEscape(want))
		u, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("parse the authorization URL: %v", err)
		}
		asked := strings.Fields(u.Query().Get("scope"))
		if len(asked) != 3 || !slices.Contains(asked, want) {
			t.Errorf("%s=: asked for %v; want exactly %q plus offline access and openid", param, asked, want)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin?scopes="+
		url.QueryEscape("499b84ac-1321-427f-aa17-267ca6975798/vso.serviceendpoint_manage"), nil)
	r = r.WithContext(withOIDCHuman(r.Context(), f.fake.Subject()))
	w := httptest.NewRecorder()
	f.srv.handleADOSignIn(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a scope outside the row's ceiling: status %d; want 400", w.Code)
	}
}

// TestADOCapture_StoresUnderTheCallersOwnPrincipal is the happy path: the
// credential lands in the caller's OWN namespace, carries the granted scopes
// the authority reported, holds no access token, and the capture is audited.
func TestADOCapture_StoresUnderTheCallersOwnPrincipal(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()

	w := f.capture(t, subject)
	if w.Code != http.StatusFound || w.Header().Get("Location") != adoSignInDonePath {
		t.Fatalf("capture: status %d location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	blob, found := f.stored(t, subject)
	if !found {
		t.Fatal("nothing was stored under the caller's own principal")
	}
	if blob.RefreshToken == "" || blob.Subject != subject || blob.TenantID != f.cfg.TenantID || blob.ClientID != f.cfg.ClientID {
		t.Fatalf("the stored blob does not name what was captured: %+v", blob)
	}
	if !blob.CapturedAt.Equal(adoTestNow) {
		t.Errorf("CapturedAt = %v; want the fixed clock", blob.CapturedAt)
	}
	if blob.Source != adoEntraSourceSignIn {
		t.Errorf("Source = %q; want %q", blob.Source, adoEntraSourceSignIn)
	}
	for _, want := range f.cfg.Scopes {
		if !slices.Contains(blob.Scopes, want) {
			t.Errorf("the stored consent does not carry %q (has %v)", want, blob.Scopes)
		}
	}
	// The blob's JSON must not carry an access token under any key: the stored
	// credential is the refresh token alone.
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	if strings.Contains(string(raw), "access_token") {
		t.Errorf("the stored blob carries an access token: %s", raw)
	}
	// The refresh token is registered with the mask registry, so it cannot
	// surface in a PTY capture.
	registered := false
	for _, v := range f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil) {
		if string(v) == blob.RefreshToken {
			registered = true
		}
	}
	if !registered {
		t.Error("the captured refresh token is not registered with the mask registry")
	}
	rows := f.audit.find(adoSignInCapturedAction)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != subject {
		t.Fatalf("audit rows = %+v; want one success row attributed to the caller", rows)
	}
	if got := rows[0].Target; got != adoEntraSecretName(f.cfg.RowID) {
		t.Errorf("audit Target = %q; want the reserved secret name", got)
	}
	if strings.Contains(string(rows[0].Data), blob.RefreshToken) {
		t.Error("the audit row carries the refresh token")
	}
}

// TestADOCapture_RefusesASubjectMismatch IS THE SECURITY PROPERTY of this lane.
//
// The tenant signs an identity token for somebody else — the shape a mixed-up
// browser session, a second tab or a deliberately replayed redirect produces —
// and the capture must refuse it and store NOTHING. Without the subject
// comparison in bindADOEntraIdentity this test's 403 becomes a 302 to the
// success page and one person's Azure DevOps reach lands in another person's
// namespace.
func TestADOCapture_RefusesASubjectMismatch(t *testing.T) {
	f := newADOFixture(t)
	const sessionSubject = "the-person-at-the-keyboard"
	f.fake.SetSubject("a-completely-different-person")

	w := f.capture(t, sessionSubject)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d body %q; want 403 — the capture must be refused", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), adoSignInSubjectMismatchRefusal) {
		t.Errorf("the refusal does not name its reason: %q", w.Body.String())
	}
	if _, found := f.stored(t, sessionSubject); found {
		t.Fatal("a credential was stored under the session subject despite the mismatch")
	}
	if _, found := f.stored(t, "a-completely-different-person"); found {
		t.Fatal("a credential was stored under the identity token's subject")
	}
	rows := f.audit.find(adoSignInCapturedAction)
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v; want one failure row", rows)
	}
	if !strings.Contains(string(rows[0].Data), "identity_binding") {
		t.Errorf("the failure row does not name the binding as the cause: %s", rows[0].Data)
	}
}

// TestADOCapture_RefusesACodeChallengeMismatch: the callback exchanges with the
// verifier from ITS OWN cookie, so a callback whose cookie does not match the
// challenge the authorization request carried cannot complete. This is the PKCE
// property end to end — the authority refuses the exchange and nothing is
// stored.
func TestADOCapture_RefusesACodeChallengeMismatch(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()

	authURL, cookies := f.signIn(t, subject, "")
	q := follow(t, authURL)
	for _, c := range cookies {
		if c.Name == adoPKCECookieName {
			c.Value = "a-different-verifier-000000000000000000000000000000000"
		}
	}
	w := f.callback(t, subject, q, cookies)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d body %q; want a 302 back to the console", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), adoSignInErrorPath+adoCaptureExchangeFailed; got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
	if _, found := f.stored(t, subject); found {
		t.Fatal("a credential was stored despite a code-challenge mismatch")
	}
}

// TestADOCapture_RefusesAForgedState: the state must match the cookie this
// server set, and a callback that does not is answered in band rather than
// routed back into a retry loop.
func TestADOCapture_RefusesAForgedState(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	authURL, cookies := f.signIn(t, subject, "")
	q := follow(t, authURL)

	t.Run("forged", func(t *testing.T) {
		forged := url.Values{"code": {q.Get("code")}, "state": {"not-the-state-we-set"}}
		w := f.callback(t, subject, forged, cookies)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d; want 400", w.Code)
		}
	})
	t.Run("no cookies", func(t *testing.T) {
		w := f.callback(t, subject, q, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d; want 400", w.Code)
		}
	})
	if _, found := f.stored(t, subject); found {
		t.Fatal("a credential was stored on a callback that proved nothing")
	}
}

// TestADOSignIn_RefusesANonLoginClientID is the 0.7.10 boundary, at BOTH doors:
// a row configured against another application is refused by name, because its
// identity token's subject is per-application and could not be bound to this
// session at all.
func TestADOSignIn_RefusesANonLoginClientID(t *testing.T) {
	f := newADOFixture(t)
	f.cfg.ClientID = "ffffffff-0000-1111-2222-333333333333"

	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		path    string
	}{
		{"signin", f.srv.handleADOSignIn, "/api/v1/scm/azure-devops/signin"},
		{"callback", f.srv.handleADOCallback, "/api/v1/scm/azure-devops/callback?code=x&state=y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = r.WithContext(withOIDCHuman(r.Context(), "someone"))
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %q; want 400", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "not the application this console signs people in with") {
				t.Errorf("the refusal does not name its reason: %q", w.Body.String())
			}
		})
	}

	// A row that names the login tenant but no login application at all is
	// refused too: an unanswerable comparison must never widen anything.
	f.cfg.ClientID = f.fake.ClientID()
	f.cfg.LoginClientID = ""
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "someone"))
	w := httptest.NewRecorder()
	f.srv.handleADOSignIn(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown login application: status %d; want 400", w.Code)
	}
}

// TestADOSignIn_RefusesACallerWithNoSessionSubject: an admin-token caller has
// no identity provider subject, so there is nothing a capture could be bound to
// and no honest namespace to store it under.
func TestADOSignIn_RefusesACallerWithNoSessionSubject(t *testing.T) {
	f := newADOFixture(t)
	for _, path := range []string{"/api/v1/scm/azure-devops/signin", "/api/v1/scm/azure-devops/callback"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		if strings.HasSuffix(path, "signin") {
			f.srv.handleADOSignIn(w, r)
		} else {
			f.srv.handleADOCallback(w, r)
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status %d; want 403", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), adoSignInNoSessionRefusal) {
			t.Errorf("%s: the refusal does not name its reason: %q", path, w.Body.String())
		}
	}
}

// TestADORedeem_GrantedScopeIsTheWholeConsentedSet pins ENTRA'S OWN BEHAVIOUR,
// measured against a real tenant: a redemption that REQUESTS one scope is
// answered with a token carrying every scope the person consented to.
//
// It is asserted here rather than left implicit because the opposite belief is
// dangerous. A reader who thinks this helper mints a narrow token will think
// the token bounds what a run may do at Azure DevOps, and it does not — the
// token is the person's own identity with all of their consent, and the layer
// that bounds a run is Wardyn's own capability check in front of the resource.
// What this helper owes its caller is the GRANTED set, which is the only honest
// record of what the token can do.
//
// The subset still goes out on the wire, and that is deliberate: it is what a
// correct client sends and what would narrow if Microsoft ever changes this.
func TestADORedeem_GrantedScopeIsTheWholeConsentedSet(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}

	subset := []string{f.cfg.Scopes[0]}
	access, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, subset)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	for _, want := range f.cfg.Scopes {
		if !slices.Contains(access.Scopes, want) {
			t.Fatalf("granted scopes = %v; want every consented scope, including %q — "+
				"this service does not narrow a token to the requested subset", access.Scopes, want)
		}
	}
	// And the helper's answer agrees with the authority's own bookkeeping,
	// which is the only way to check an opaque token.
	carried, ok := f.fake.ScopesForAccessToken(access.AccessToken)
	if !ok {
		t.Fatal("the authority does not recognise the token it issued")
	}
	if !slices.Equal(carried, access.Scopes) {
		t.Fatalf("the helper reported %v but the token carries %v", access.Scopes, carried)
	}
	if want := adoTestNow.Add(time.Hour); !access.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v; want now+expires_in (%v)", access.ExpiresAt, want)
	}
	// The stored CONSENT is untouched by a narrowed request: the next
	// redemption may still name a scope this one left out.
	blob, found := f.stored(t, subject)
	if !found {
		t.Fatal("the credential vanished")
	}
	for _, want := range f.cfg.Scopes {
		if !slices.Contains(blob.Scopes, want) {
			t.Errorf("a narrowed request narrowed the stored consent: %v", blob.Scopes)
		}
	}
}

// TestADORedeem_SendsTheRequestedSubsetOnTheWire: the request still names the
// least the caller needs. The authority ignores it today, so nothing observable
// downstream depends on it — which is exactly why it needs its own assertion,
// or it would rot away unnoticed and there would be nothing to narrow the day
// this behaviour changes.
func TestADORedeem_SendsTheRequestedSubsetOnTheWire(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}

	// A proxy in front of the tenant that records the scope the request named.
	var asked string
	var mu sync.Mutex
	upstream := f.cfg.AuthorityOverride
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if form, err := url.ParseQuery(string(body)); err == nil && form.Get("grant_type") == "refresh_token" {
			mu.Lock()
			asked = form.Get("scope")
			mu.Unlock()
		}
		req, err := http.NewRequest(r.Method, upstream+r.URL.Path, strings.NewReader(string(body)))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer spy.Close()
	f.cfg.AuthorityOverride = spy.URL

	subset := []string{f.cfg.Scopes[0]}
	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, subset); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != strings.Join(subset, " ") {
		t.Fatalf("the request named scope %q; want exactly the subset %v", asked, subset)
	}
}

// TestADORedeem_RotationIsPersistedAndTheOldTokenRejected is why the control
// plane owns refresh: the authority rotates the refresh token on every
// redemption, the rotation must land in the store, and the token that was
// presented must stop working.
func TestADORedeem_RotationIsPersistedAndTheOldTokenRejected(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	before, _ := f.stored(t, subject)

	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	after, found := f.stored(t, subject)
	if !found {
		t.Fatal("the credential vanished")
	}
	if after.RefreshToken == before.RefreshToken {
		t.Fatal("the rotated refresh token was not persisted")
	}
	if !after.RenewedAt.Equal(adoTestNow) {
		t.Errorf("RenewedAt = %v; want the fixed clock", after.RenewedAt)
	}
	if live, known := f.fake.RefreshTokenState(before.RefreshToken); !known || live {
		t.Fatalf("the presented token should be retired at the authority (live=%v known=%v)", live, known)
	}

	// Put the SPENT token back in the store, the state a control plane that
	// failed to persist a rotation would be in, and confirm the next redemption
	// classifies it as a dead credential rather than as an outage.
	if err := f.srv.storeADOEntraBlob(context.Background(), subject, f.cfg.RowID, before); err != nil {
		t.Fatalf("restore the spent blob: %v", err)
	}
	_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
	if !errors.Is(err, ErrADOEntraDeadCredential) {
		t.Fatalf("redeeming a spent token: err = %v; want a dead credential", err)
	}
	if got := ADOEntraClassify(err); got != ADOEntraFailureDeadCredential {
		t.Errorf("classification = %q; want %q", got, ADOEntraFailureDeadCredential)
	}
}

// TestADORedeem_OneRefreshAtATimePerOwner: the refresh token rotates, so two
// concurrent redemptions that both read the pre-rotation blob would spend the
// same token twice and one would be told its perfectly good credential is dead.
//
// All eight redemptions SUCCEEDING is the proof of single-flight: they can only
// all succeed if each one read the token the previous one persisted, which is
// only possible under the per-owner lock. Run under the race detector, this
// also covers the lock registry itself.
func TestADORedeem_OneRefreshAtATimePerOwner(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}

	const redemptions = 8
	var wg sync.WaitGroup
	errs := make([]error, redemptions)
	tokens := make([]string, redemptions)
	for i := range redemptions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			access, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
			errs[i], tokens[i] = err, access.AccessToken
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("redemption %d failed, so two of them raced for one rotating token: %v", i, err)
		}
	}
	// Every redemption got its own token, and the one the store now holds is
	// live at the authority.
	seen := map[string]bool{}
	for _, tok := range tokens {
		if tok == "" || seen[tok] {
			t.Fatalf("two redemptions were served the same access token: %v", tokens)
		}
		seen[tok] = true
	}
	blob, found := f.stored(t, subject)
	if !found {
		t.Fatal("the credential vanished")
	}
	if live, known := f.fake.RefreshTokenState(blob.RefreshToken); !known || !live {
		t.Fatalf("the stored refresh token is not the live one (live=%v known=%v)", live, known)
	}
}

// TestADORedeem_RefusesDotDefaultAndAnUnconsentedScope: a redemption may name
// only what the capture consented to, and may never name `.default` — which is
// not a narrow scope but a request for every permission the application holds.
func TestADORedeem_RefusesDotDefaultAndAnUnconsentedScope(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}

	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject,
		[]string{"499b84ac-1321-427f-aa17-267ca6975798/.default"}); err == nil ||
		!strings.Contains(err.Error(), "every permission the app registration holds") {
		t.Fatalf(".default: err = %v; want a refusal naming why", err)
	}
	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, nil); err == nil {
		t.Fatal("a redemption that named no scope was accepted")
	}
	_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject,
		[]string{"499b84ac-1321-427f-aa17-267ca6975798/vso.serviceendpoint_manage"})
	if !errors.Is(err, ErrADOEntraConsentRequired) {
		t.Fatalf("an unconsented scope: err = %v; want a consent refusal", err)
	}
}

// TestADORedeem_ErrorClassification grades the four classes a caller answers
// differently. Collapsing any pair of them turns one authority hiccup
// into a fleet-wide re-sign-in, which is why each has to be provable.
func TestADORedeem_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(*entrafake.Server)
		want ADOEntraFailure
		is   error
	}{
		{"dead credential", func(s *entrafake.Server) { s.SetInvalidGrant(true) }, ADOEntraFailureDeadCredential, ErrADOEntraDeadCredential},
		{"consent required", func(s *entrafake.Server) { s.SetConsentRequired(true) }, ADOEntraFailureConsentRequired, ErrADOEntraConsentRequired},
		{"interaction required", func(s *entrafake.Server) { s.SetInteractionRequired(true) }, ADOEntraFailureInteractionRequired, ErrADOEntraInteractionRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADOFixture(t)
			subject := f.fake.Subject()
			if w := f.capture(t, subject); w.Code != http.StatusFound {
				t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
			}
			tc.arm(f.fake)
			_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
			if !errors.Is(err, tc.is) {
				t.Fatalf("err = %v; want %v", err, tc.is)
			}
			if got := ADOEntraClassify(err); got != tc.want {
				t.Fatalf("classification = %q; want %q", got, tc.want)
			}
			// A dead refresh token is deleted (CS-5: nothing can renew it);
			// every other class leaves the credential in place, since consent
			// or a person at a keyboard can still make it work.
			_, found := f.stored(t, subject)
			if dead := tc.want == ADOEntraFailureDeadCredential; found == dead {
				t.Errorf("stored credential present = %v after a %s refusal; want %v", found, tc.want, !dead)
			}
		})
	}

	// The unavailable class: an authority that cannot be reached at all.
	t.Run("unavailable", func(t *testing.T) {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		if w := f.capture(t, subject); w.Code != http.StatusFound {
			t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
		}
		f.fake.Close()
		_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
		if !errors.Is(err, ErrADOEntraUnavailable) {
			t.Fatalf("err = %v; want the unavailable class", err)
		}
	})

	// And the not-captured class, which is not an authority answer at all.
	t.Run("not captured", func(t *testing.T) {
		f := newADOFixture(t)
		_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, "nobody-has-signed-in", f.cfg.Scopes)
		if !errors.Is(err, ErrADOEntraNotCaptured) {
			t.Fatalf("err = %v; want the not-captured class", err)
		}
	})
}

// TestADOEntraBlobIsNotReadableByAnotherPrincipal is the store-level property
// the List-then-Get read exists for.
//
// The owner view's Get FALLS BACK to the operator's row by contract, so both
// halves matter: one person's capture must not be readable by another, and an
// OPERATOR's blob must not be served to a member who has captured nothing —
// which is exactly what a bare For(owner).Get would do.
func TestADOEntraBlobIsNotReadableByAnotherPrincipal(t *testing.T) {
	f := newADOFixture(t)
	ctx := context.Background()
	const alice, bob = "alice-subject", "bob-subject"

	if err := f.srv.storeADOEntraBlob(ctx, alice, f.cfg.RowID, adoEntraBlob{
		RefreshToken: "alices-refresh-token", Scopes: f.cfg.Scopes,
		TenantID: f.cfg.TenantID, ClientID: f.cfg.ClientID, Subject: alice,
	}); err != nil {
		t.Fatalf("store alice's sign-in: %v", err)
	}
	if blob, found := f.stored(t, alice); !found || blob.RefreshToken != "alices-refresh-token" {
		t.Fatalf("alice cannot read her own credential: found=%v blob=%+v", found, blob)
	}
	if _, found := f.stored(t, bob); found {
		t.Fatal("bob can read alice's captured Azure DevOps sign-in")
	}

	// An operator-namespace blob at the same reserved name must not be served
	// to a member either.
	raw, err := json.Marshal(adoEntraBlob{
		RefreshToken: "the-operators-refresh-token", Scopes: f.cfg.Scopes,
		TenantID: f.cfg.TenantID, ClientID: f.cfg.ClientID, Subject: "the-operator",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := f.srv.cfg.Secrets.Put(ctx, adoEntraSecretName(f.cfg.RowID), raw); err != nil {
		t.Fatalf("seed the operator namespace: %v", err)
	}
	if blob, found := f.stored(t, bob); found {
		t.Fatalf("a member was served the operator's credential: %+v", blob)
	}
	// And a read with no principal at all resolves nothing, rather than the
	// operator's row.
	if _, found, err := f.srv.readADOEntraBlob(ctx, "", f.cfg.RowID); err != nil || found {
		t.Fatalf("an ownerless read resolved something: found=%v err=%v", found, err)
	}
}

// TestADOEntraSecretNameIsReserved: the generated name must land inside the
// reserved-name pattern, or the generic secrets API could overwrite, delete or
// list a captured credential and a credential sink could resolve it as a raw
// value.
func TestADOEntraSecretNameIsReserved(t *testing.T) {
	name := adoEntraSecretName("ado-row-1")
	if !reservedSecret(name) {
		t.Fatalf("%q is not a reserved secret name", name)
	}
	if !sinkReservedSecret(name) {
		t.Fatalf("%q is not refused at the credential sinks", name)
	}
	for _, bad := range []string{"", "-leading", "row/../other", "row oauth", "row-oauth", "wardyn-harness-x", strings.Repeat("a", 65)} {
		if adoEntraValidRowID(bad) {
			t.Errorf("row id %q was accepted as a store name", bad)
		}
	}
}

// TestEntraAuthorityOverrideRequiresTheTestHatch: the override is refused, not
// ignored, without WARDYN_ALLOW_TEST_ENDPOINTS — and every shape a typo could
// hide is refused even with it.
func TestEntraAuthorityOverrideRequiresTheTestHatch(t *testing.T) {
	if _, err := ValidateEntraAuthorityOverride("http://127.0.0.1:9999", false); err == nil {
		t.Fatal("the override was accepted without the test-endpoint acknowledgement")
	}
	got, err := ValidateEntraAuthorityOverride("http://127.0.0.1:9999/", true)
	if err != nil || got != "http://127.0.0.1:9999" {
		t.Fatalf("with the hatch: %q, %v; want the normalized base URL", got, err)
	}
	if got, err := ValidateEntraAuthorityOverride("", false); got != "" || err != nil {
		t.Fatalf("empty: %q, %v; want no override and no error", got, err)
	}
	for _, bad := range []string{"login.microsoftonline.com", "ftp://host", "https://u:p@host", "https://host/path", "https://host?q=1", "https://host#f"} {
		if _, err := ValidateEntraAuthorityOverride(bad, true); err == nil {
			t.Errorf("%q was accepted as an authority", bad)
		}
	}
	// A configuration carrying the override WITHOUT the acknowledgement is
	// refused at the door, not silently pointed at the real Microsoft endpoint.
	f := newADOFixture(t)
	f.cfg.AllowTestEndpoints = false
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "someone"))
	w := httptest.NewRecorder()
	f.srv.handleADOSignIn(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d body %q; want the override refused", w.Code, w.Body.String())
	}
}

// TestADOSignIn_RefusesAnUnconfiguredDeployment: with no source wired, both
// doors say so rather than 404ing as an unknown path.
func TestADOSignIn_RefusesAnUnconfiguredDeployment(t *testing.T) {
	s := &Server{cfg: Config{Now: func() time.Time { return adoTestNow }}}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "someone"))
	w := httptest.NewRecorder()
	s.handleADOSignIn(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), adoSignInUnconfiguredRefusal) {
		t.Fatalf("status %d body %q; want the not-configured refusal", w.Code, w.Body.String())
	}
}
