// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// testLoginHMACKey is the session-cookie key the login-flow fixtures sign with.
// Length is what oidc.New enforces; the value is irrelevant.
var testLoginHMACKey = []byte("0123456789abcdef0123456789abcdef")

// the request half

// TestLoginScopesAreEmptyWithoutAConfiguredRow IS THE NO-OP GUARANTEE on this
// side of the seam: a deployment with no Azure DevOps row asks the login for
// nothing extra, so its authorization request is never widened and nothing an
// operator can see changes.
//
// Every arm here is a deployment that exists today: no source wired at all
// (every installation before this row type existed), a source that reports no
// row (the row was never created, or is disabled), and a source that errors (a
// store hiccup). All three answer nil, because the login must be unaffected by
// anything going on over here.
func TestLoginScopesAreEmptyWithoutAConfiguredRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  ADOEntraSource
	}{
		{"no source wired", nil},
		{"source reports no row", func(context.Context) (ADOEntraConfig, bool, error) {
			return ADOEntraConfig{}, false, nil
		}},
		{"source errors", func(context.Context) (ADOEntraConfig, bool, error) {
			return ADOEntraConfig{}, false, context.DeadlineExceeded
		}},
		{"row is unusable", func(context.Context) (ADOEntraConfig, bool, error) {
			return ADOEntraConfig{RowID: "ado-row-1"}, true, nil // no tenant, client or scopes
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{ADOEntra: tc.src, Now: func() time.Time { return adoTestNow }}}
			if got := s.LoginScopes(context.Background()); got != nil {
				t.Fatalf("LoginScopes = %v; want nil — this deployment's login request must not move", got)
			}
		})
	}
}

// TestLoginScopesCarryTheCeilingForTheLoginApplication: a row naming the
// console's own sign-in application, in the sign-in tenant, widens the login
// with the whole ceiling plus offline access.
//
// The WHOLE ceiling and not a subset: consent is the only narrowing that has
// any effect on this service, so what is consented at login is the reach the
// organisation is choosing to grant, and anything left out would cost a second
// consent prompt later.
func TestLoginScopesCarryTheCeilingForTheLoginApplication(t *testing.T) {
	f := newADOFixture(t)
	got := f.srv.LoginScopes(context.Background())
	want := append(slices.Clone(f.cfg.Scopes), entraOfflineAccessScope)
	if !slices.Equal(got, want) {
		t.Fatalf("LoginScopes = %v; want %v", got, want)
	}
	if slices.Contains(got, entraOpenIDScope) {
		t.Error("openid is already in the login's base scopes and must not be added again")
	}
}

// TestLoginScopesAreEmptyForAnotherTenantOrApplication is the 0.7.10 boundary
// on the login path.
//
// A row against a different application would have the login request consent
// for scopes whose token this deployment could never bind to a session: one
// authorization request yields one id_token from one app registration, and the
// subject that binds a credential is per-application. Those deployments are not
// broken — they use the dedicated sign-in, which was built for exactly this —
// but their console login must not be widened.
func TestLoginScopesAreEmptyForAnotherTenantOrApplication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mangle func(*ADOEntraConfig)
	}{
		{"another application", func(c *ADOEntraConfig) { c.ClientID = "ffffffff-0000-1111-2222-333333333333" }},
		{"another tenant", func(c *ADOEntraConfig) { c.TenantID = "99999999-8888-7777-6666-555555555555" }},
		{"no login application known", func(c *ADOEntraConfig) { c.LoginClientID = "" }},
		{"no login tenant known", func(c *ADOEntraConfig) { c.LoginTenantID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADOFixture(t)
			tc.mangle(&f.cfg)
			if got := f.srv.LoginScopes(context.Background()); got != nil {
				t.Fatalf("LoginScopes = %v; want nil — the console login must not be widened for another application", got)
			}
		})
	}
}

// the capture half, in isolation

// TestCaptureLoginGrant_StoresUnderTheSubject: the credential the login earned
// lands in that person's own namespace, carries the granted scopes, and is
// marked as having come from the organisation's sign-in rather than from a
// person's own errand.
func TestCaptureLoginGrant_StoresUnderTheSubject(t *testing.T) {
	f := newADOFixture(t)
	const subject = "a-person"
	granted := strings.Join(append([]string{"openid", "profile", "email", entraOfflineAccessScope}, f.cfg.Scopes...), " ")

	f.srv.CaptureLoginGrant(context.Background(), subject, oidc.LoginGrant{
		RefreshToken: "the-refresh-token-this-login-earned",
		Scope:        granted,
		Expiry:       adoTestNow.Add(time.Hour),
	})

	blob, found := f.stored(t, subject)
	if !found {
		t.Fatal("the login earned a credential and nothing was stored")
	}
	if blob.RefreshToken != "the-refresh-token-this-login-earned" || blob.Subject != subject {
		t.Fatalf("the stored blob does not name what was captured: %+v", blob)
	}
	if blob.Source != adoEntraSourceLogin {
		t.Fatalf("Source = %q; want %q — the rest of the system has to tell the two paths apart",
			blob.Source, adoEntraSourceLogin)
	}
	if !slices.Equal(blob.Scopes, f.cfg.Scopes) {
		t.Errorf("stored scopes = %v; want the row's scopes that were granted %v", blob.Scopes, f.cfg.Scopes)
	}
	// Nobody else can read it, exactly as on the dedicated path.
	if _, found := f.stored(t, "somebody-else"); found {
		t.Error("another principal can read the credential this login captured")
	}
	registered := false
	for _, v := range f.srv.cfg.MaskRegistry.Snapshot(uuidNil()) {
		if string(v) == blob.RefreshToken {
			registered = true
		}
	}
	if !registered {
		t.Error("the captured refresh token is not registered with the mask registry")
	}
	rows := f.audit.find(adoSignInCapturedAction)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != subject {
		t.Fatalf("audit rows = %+v; want one success row attributed to the person", rows)
	}
	if !strings.Contains(string(rows[0].Data), `"source":"`+adoEntraSourceLogin+`"`) {
		t.Errorf("the audit row does not name the path that captured it: %s", rows[0].Data)
	}
}

// TestCaptureLoginGrant_StoresNothingWithoutAzureDevOpsScopes IS THE
// LOCK-OUT-AVOIDANCE PROPERTY, on the storage side.
//
// Consent declined, a Conditional Access policy, a tenant that will not issue
// these scopes — all of them arrive here as a grant whose scope string names
// none of the row's scopes. Nothing is stored and nothing is refused: the
// person is signed into the console and simply has no Azure DevOps credential
// yet. There is no audit row either, because this is one event per login per
// person on a tenant that never issues them, and the absence of a blob already
// says everything a row would.
func TestCaptureLoginGrant_StoresNothingWithoutAzureDevOpsScopes(t *testing.T) {
	for _, tc := range []struct{ name, scope, refresh string }{
		{"consent declined for the resource", "openid profile email offline_access", "a-refresh-token"},
		{"no scope reported at all", "", "a-refresh-token"},
		{"no refresh token issued", "openid profile email", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADOFixture(t)
			f.srv.CaptureLoginGrant(context.Background(), "a-person", oidc.LoginGrant{
				RefreshToken: tc.refresh, Scope: tc.scope, Expiry: adoTestNow.Add(time.Hour),
			})
			if _, found := f.stored(t, "a-person"); found {
				t.Fatal("a credential was stored from a grant that carried no Azure DevOps scope")
			}
			if rows := f.audit.find(adoSignInCapturedAction); len(rows) != 0 {
				t.Fatalf("audit rows = %+v; want none — a declined consent is not an event worth one per login", rows)
			}
		})
	}
}

// TestCaptureLoginGrant_IgnoresAnotherApplicationsRow: the same boundary as the
// request half. A grant from the console login can only ever be bound under a
// row that names the console's own application, so a row naming another one
// captures nothing and leaves that deployment on the dedicated sign-in.
func TestCaptureLoginGrant_IgnoresAnotherApplicationsRow(t *testing.T) {
	f := newADOFixture(t)
	f.cfg.ClientID = "ffffffff-0000-1111-2222-333333333333"
	f.srv.CaptureLoginGrant(context.Background(), "a-person", oidc.LoginGrant{
		RefreshToken: "a-refresh-token",
		Scope:        strings.Join(f.cfg.Scopes, " "),
		Expiry:       adoTestNow.Add(time.Hour),
	})
	if _, found := f.stored(t, "a-person"); found {
		t.Fatal("a credential was stored under a row naming another application")
	}
}

// the whole login, end to end

// loginFixture is a REAL Authenticator pointed at the Entra fake, with the
// server attached as its login-grant sink — the wiring cmd/wardynd does at
// boot, exercised here so the claim "signing into Wardyn connects you" is
// proven by a login rather than by its parts.
type loginFixture struct {
	*adoFixture
	auth *oidc.Authenticator
}

func newLoginFixture(t *testing.T, attach bool) *loginFixture {
	t.Helper()
	f := newADOFixture(t)
	// The console signs in against the SAME tenant and application the Azure
	// DevOps row names — the configuration this whole path is for.
	redirect := "http://console.example.invalid/auth/callback"
	f.fake.SetRedirectURI(redirect)
	f.cfg.RedirectURL = redirect

	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:   f.fake.Issuer(),
		ClientID:    f.fake.ClientID(),
		RedirectURL: redirect,
		// Any signed-in human is a member here: this fixture is about the
		// credential, and role derivation has its own suite.
		DefaultRole: oidc.RoleMember,
	}, testLoginHMACKey)
	if err != nil {
		t.Fatalf("oidc.New against the fake tenant: %v", err)
	}
	if attach {
		auth.AttachLoginGrantSink(f.srv)
	}
	return &loginFixture{adoFixture: f, auth: auth}
}

// login drives the whole browser dance: LoginHandler, the tenant's authorize
// leg, then CallbackHandler with the cookies the first leg set.
func (lf *loginFixture) login(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	lf.auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("LoginHandler: status %d body %q", w.Code, w.Body.String())
	}
	q := follow(t, w.Header().Get("Location"))
	// A refusal is carried through verbatim rather than fatal'd on: what the
	// callback DOES with one is the property several of these cases are about.
	params := url.Values{"state": {q.Get("state")}}
	for _, k := range []string{"code", "error", "error_description"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+params.Encode(), nil)
	for _, c := range w.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	lf.auth.CallbackHandler(out, cb)
	return out
}

// loginWithRetry is login plus the ONE unwidened retry the callback may issue
// when the tenant refuses the widened request — the browser hop a real person
// would make without noticing.
func (lf *loginFixture) loginWithRetry(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := lf.login(t)
	if sessionIssued(w) {
		return w
	}
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/authorize") {
		return w // not a retry; hand the caller what actually happened
	}
	return lf.followRetry(t, w)
}

// followRetry walks the authorization redirect a retry issued and drives the
// callback it lands on, carrying the retry's own fresh cookies.
func (lf *loginFixture) followRetry(t *testing.T, retry *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	q := follow(t, retry.Header().Get("Location"))
	params := url.Values{"state": {q.Get("state")}}
	for _, k := range []string{"code", "error", "error_description"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+params.Encode(), nil)
	for _, c := range latestCookies(retry) {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	lf.auth.CallbackHandler(out, cb)
	return out
}

// latestCookies keeps the LAST Set-Cookie header per name, the way a browser
// applies them. A retry response carries each cookie twice — the expiring copy
// the callback cleared, then the fresh one the new login leg set — and a naive
// replay would send the empty one first, which is what the handler would read.
func latestCookies(w *httptest.ResponseRecorder) []*http.Cookie {
	byName := map[string]*http.Cookie{}
	var order []string
	for _, c := range w.Result().Cookies() {
		if _, seen := byName[c.Name]; !seen {
			order = append(order, c.Name)
		}
		byName[c.Name] = c
	}
	out := make([]*http.Cookie, 0, len(order))
	for _, n := range order {
		if byName[n].Value != "" {
			out = append(out, byName[n])
		}
	}
	return out
}

// sessionIssued reports whether the response wrote a Wardyn session cookie with
// a value — the one observable that says this person is signed in.
func sessionIssued(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return true
		}
	}
	return false
}

// TestConsoleLoginCapturesAzureDevOpsAccess is the OWNER REQUIREMENT, end to
// end: an admin configures the row once and a member who signs into Wardyn is
// connected to Azure DevOps with nothing else to do.
//
// It drives a real Authenticator against a real (fake) tenant: the login
// request carries the Azure DevOps scopes, the tenant consents, the callback
// signs the person in AND the refresh token lands under their own principal,
// marked as having come from the organisation's sign-in.
func TestConsoleLoginCapturesAzureDevOpsAccess(t *testing.T) {
	lf := newLoginFixture(t, true)

	w := lf.login(t)
	if !sessionIssued(w) {
		t.Fatalf("the login did not issue a session: status %d location %q body %q",
			w.Code, w.Header().Get("Location"), w.Body.String())
	}
	blob, found := lf.stored(t, lf.fake.Subject())
	if !found {
		t.Fatal("signing into the console did not connect Azure DevOps — this is the whole requirement")
	}
	if blob.Source != adoEntraSourceLogin {
		t.Errorf("Source = %q; want %q", blob.Source, adoEntraSourceLogin)
	}
	if blob.Subject != lf.fake.Subject() {
		t.Errorf("the credential is bound to %q, not to the person who signed in", blob.Subject)
	}
	for _, want := range lf.cfg.Scopes {
		if !slices.Contains(blob.Scopes, want) {
			t.Errorf("the captured consent does not carry %q (has %v)", want, blob.Scopes)
		}
	}
	// It is a USABLE credential, not merely a stored one: the control plane can
	// redeem it without anyone signing in again.
	access, err := lf.srv.RedeemADOEntraAccess(context.Background(), lf.cfg, lf.fake.Subject(), lf.cfg.Scopes)
	if err != nil {
		t.Fatalf("the credential the login captured cannot be redeemed: %v", err)
	}
	if access.AccessToken == "" || len(access.Scopes) == 0 {
		t.Fatalf("redeeming it produced nothing usable: %+v", access)
	}
}

// TestConsoleLoginSucceedsWhenAzureDevOpsConsentIsDeclined IS THE ONE THAT
// Would lock an organisation out.
//
// The tenant refuses the Azure DevOps scopes — a declined consent, a policy, a
// tenant that will not issue them. The login MUST still succeed: the person
// gets into the console and simply has no Azure DevOps credential yet. A
// deployment where a second resource's consent can cost everyone their console
// session is a deployment nobody can rescue.
func TestConsoleLoginSucceedsWhenAzureDevOpsConsentIsDeclined(t *testing.T) {
	lf := newLoginFixture(t, true)
	// The tenant holds consent for the console's own scopes and for NOTHING
	// else, so it refuses the widened authorization request outright — the
	// harshest shape of this failure, and the one that would otherwise take
	// every login in the organisation down with it.
	lf.fake.SetConsentedScopes()

	w := lf.loginWithRetry(t)
	if !sessionIssued(w) {
		t.Fatalf("a declined Azure DevOps consent cost this person their console session: status %d location %q",
			w.Code, w.Header().Get("Location"))
	}
	if _, found := lf.stored(t, lf.fake.Subject()); found {
		t.Fatal("a credential was stored although the tenant granted no Azure DevOps scope")
	}
}

// TestConsoleLoginSucceedsWhenTheGrantOmitsTheScopes: the row names one scope
// the tenant consents to and one it does not. The fake refuses a request that
// names an unconsented scope rather than granting partially, so this login
// goes THROUGH THE RETRY: the widened request is refused, the unwidened retry
// signs the person in, and nothing is stored.
//
// It does NOT exercise partial consent — a successful grant that merely omits
// the Azure DevOps scopes. That branch is covered in isolation by
// TestCaptureLoginGrant_StoresNothingWithoutAzureDevOpsScopes; whether a real
// tenant answers this shape with a partial grant or an outright refusal is not
// established here, and both are handled.
func TestConsoleLoginSucceedsWhenTheGrantOmitsTheScopes(t *testing.T) {
	lf := newLoginFixture(t, true)
	// The row asks for a scope the tenant consents to, then the row is widened
	// to name one it does not — so the login completes and the granted set is
	// missing what the capture was hoping for.
	lf.cfg.Scopes = append(slices.Clone(lf.cfg.Scopes),
		"499b84ac-1321-427f-aa17-267ca6975798/vso.serviceendpoint_manage")
	lf.fake.SetConsentedScopes(lf.cfg.Scopes[0])

	w := lf.loginWithRetry(t)
	if !sessionIssued(w) {
		t.Fatalf("the login did not succeed: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	// The retry drops the extra scopes entirely, so nothing is captured — the
	// person is in the console with no Azure DevOps credential yet, which is
	// exactly the state this is supposed to degrade to.
	if _, found := lf.stored(t, lf.fake.Subject()); found {
		t.Fatal("a credential was stored from a grant that did not carry the row's scopes")
	}
}

// TestWidenedLoginRetriesOnlyOnce pins the bound on the recovery path: the
// retry is issued unwidened, so it sets no marker and its own callback cannot
// retry again. A tenant that refuses BOTH requests produces one retry and then
// an ordinary login failure, never a redirect loop.
func TestWidenedLoginRetriesOnlyOnce(t *testing.T) {
	lf := newLoginFixture(t, true)
	lf.fake.SetConsentedScopes()
	// Refuse the second, unwidened request too.
	lf.fake.SetConsentRequired(true)

	w := lf.login(t)
	if w.Code != http.StatusFound {
		t.Fatalf("the first callback did not retry: status %d", w.Code)
	}
	second := lf.followRetry(t, w)
	if sessionIssued(second) {
		t.Fatal("the tenant refused everything, so no session should have been issued")
	}
	for _, c := range second.Result().Cookies() {
		if c.Name == "wardyn_oidc_widened" && c.Value != "" && c.MaxAge >= 0 {
			t.Fatal("the retry set the widened marker again, so a third attempt would be possible")
		}
	}
}

// TestConsoleLoginWithoutTheSinkIsUnchanged: the same login, with nothing
// attached, still signs the person in and stores nothing. This is the existing
// deployment, running the existing code path.
func TestConsoleLoginWithoutTheSinkIsUnchanged(t *testing.T) {
	lf := newLoginFixture(t, false)

	w := lf.login(t)
	if !sessionIssued(w) {
		t.Fatalf("the login did not issue a session: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	if _, found := lf.stored(t, lf.fake.Subject()); found {
		t.Fatal("a credential was stored on a deployment with no sink attached")
	}
	if rows := lf.audit.find(adoSignInCapturedAction); len(rows) != 0 {
		t.Fatalf("audit rows = %+v; want none", rows)
	}
	// The CALLBACK's headers are unchanged too, not only the request: an
	// unwidened login must not write a Set-Cookie for the widened marker.
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_oidc_widened" {
			t.Fatalf("an unwidened callback wrote a Set-Cookie for the widened marker: %+v", c)
		}
	}
}

// TestConcurrentConsoleLoginsAreRaceClean: many people signing in at once all
// cross the sink — its row read, its per-person lock and its store write — on
// the request path. Under -race this pins that the seam, the bounded calls and
// the capture share nothing unsafely, and that every one of them still ends in
// a session.
func TestConcurrentConsoleLoginsAreRaceClean(t *testing.T) {
	lf := newLoginFixture(t, true)
	const logins = 48
	var wg sync.WaitGroup
	sessions := make([]bool, logins)
	for i := range logins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions[i] = sessionIssued(lf.login(t))
		}()
	}
	wg.Wait()
	for i, ok := range sessions {
		if !ok {
			t.Fatalf("login %d of %d did not issue a session", i, logins)
		}
	}
	if _, found := lf.stored(t, lf.fake.Subject()); !found {
		t.Fatal("no credential was captured by any of the concurrent logins")
	}
}

// TestDeniedLoginCapturesNothing: the sink is offered the grant only once the
// login is APPROVED, so a human this deployment refuses to sign in never has a
// credential stored for them. Checked with role derivation configured to deny
// (no default role, no map, and an identity token carrying no role claim).
func TestDeniedLoginCapturesNothing(t *testing.T) {
	f := newADOFixture(t)
	redirect := "http://console.example.invalid/auth/callback"
	f.fake.SetRedirectURI(redirect)
	f.cfg.RedirectURL = redirect

	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:   f.fake.Issuer(),
		ClientID:    f.fake.ClientID(),
		RedirectURL: redirect,
		// A role map with one row nothing in this identity token matches, and
		// NO default role: derivation finds nothing and the login is denied.
		// (An EMPTY map is not a denial — it means claim-based derivation is
		// off, and every signed-in human is an admin.)
		RoleMap: map[string]string{"a-group-nobody-is-in": oidc.RoleMember},
	}, testLoginHMACKey)
	if err != nil {
		t.Fatalf("oidc.New against the fake tenant: %v", err)
	}
	auth.AttachLoginGrantSink(f.srv)
	lf := &loginFixture{adoFixture: f, auth: auth}

	w := lf.login(t)
	if sessionIssued(w) {
		t.Fatal("the login was supposed to be denied; this test proves nothing")
	}
	if _, found := f.stored(t, f.fake.Subject()); found {
		t.Fatal("a credential was captured for a human this deployment refused to sign in")
	}
}

// uuidNil is the run id the mask registry's global snapshot is read under.
// Spelled as a helper so this file needs no uuid import of its own.
func uuidNil() [16]byte { return [16]byte{} }
