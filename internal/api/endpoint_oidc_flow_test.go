// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The flow tests run the real door and callback against test/entrafake in its generic mode, which answers as
// a plain OIDC authority: it omits `scope` from every token response and ignores one on a refresh.

const (
	oidcCallbackURL  = "http://127.0.0.1:8080/api/v1/auth/provider-oidc/callback"
	oidcCallbackPath = "/auth/provider-oidc/callback"
	oidcClientSecret = "oidc-client-secret-value-0001"
	oidcSecretRef    = "oidc-client-secret"
	oidcPersonSub    = "oidc-person-subject"
)

// oidcWire records every token value the authority hands out, read off the wire by a proxy in front of it, so a
// leak sweep knows the values it is searching for.
type oidcWire struct {
	mu   sync.Mutex
	seen []string
}

func (w *oidcWire) add(v string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, v)
}

func (w *oidcWire) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

type oidcFlow struct {
	srv   *Server
	audit *memAudit
	fake  *entrafake.Server
	site  *azureSiteStore
	mem   *memSecrets
	mux   http.Handler
	wire  *oidcWire
	logs  *lockedBuffer
	row   types.ModelProvider
	env   EndpointOIDCEnv
	// gate, when set, sees every request to the authority before it is served and may hold it.
	gate atomic.Pointer[func(*http.Request)]
}

// newOIDCFlow wires a server whose custom_endpoint row signs people in at a fake authority. binding "" is the
// default (console); the console issuer the server reports is the fake's own, so the default binding can hold.
func newOIDCFlow(t *testing.T, binding string) *oidcFlow {
	t.Helper()
	f := &oidcFlow{audit: &memAudit{}, wire: &oidcWire{}, logs: &lockedBuffer{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f.fake = entrafake.New()
	t.Cleanup(f.fake.Close)
	target, err := url.Parse(f.fake.URL())
	if err != nil {
		t.Fatalf("parse the fake's URL: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = func(resp *http.Response) error {
		if !strings.HasSuffix(resp.Request.URL.Path, "/token") {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			for _, k := range []string{"access_token", "refresh_token", "id_token"} {
				if v, ok := m[k].(string); ok && v != "" {
					f.wire.add(v)
				}
			}
		}
		return nil
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g := f.gate.Load(); g != nil {
			(*g)(r)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	f.fake.SetBaseURL(proxy.URL)
	f.fake.SetGenericMode(true)
	f.fake.SetClientSecret(oidcClientSecret)
	f.fake.SetRedirectURI(oidcCallbackURL)
	f.fake.SetSubject(oidcPersonSub)

	f.row = endpointProvider()
	f.row.UID = oidcUID
	f.row.OIDC = &types.ProviderOIDC{
		Issuer: f.fake.Issuer(), ClientID: f.fake.ClientID(), ClientSecretRef: oidcSecretRef,
		Scopes: []string{"api.read"}, IdentityBinding: binding,
	}
	f.env = EndpointOIDCEnv{ConsoleIssuer: f.fake.Issuer(), CallbackURL: oidcCallbackURL}
	f.site = &azureSiteStore{sc: types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{f.row}}}}
	f.mem = &memSecrets{m: map[string][]byte{oidcSecretRef: []byte(oidcClientSecret)}}
	f.srv = &Server{cfg: Config{
		Store:              f.site,
		Secrets:            f.mem,
		MaskRegistry:       secretmask.NewRegistry(),
		Now:                func() time.Time { return adoTestNow },
		Audit:              f.audit,
		AllowTestEndpoints: true,
		EndpointOIDC:       func() (EndpointOIDCEnv, bool) { return f.env, true },
	}}
	f.srv.cfg.OIDC = newOIDCStampAuth(t, bytes.Repeat([]byte{9}, 32))
	mux := chi.NewRouter()
	f.srv.mountModelProviderCredentialRoutes(mux)
	f.mux = mux
	return f
}

// newOIDCStampAuth is a console authenticator over a throwaway discovery server, holding key: what signs the
// sign-in's state cookie. Two of them with the same key are two replicas.
func newOIDCStampAuth(t *testing.T, key []byte) *oidc.Authenticator {
	t.Helper()
	idp := &gooidctest.Server{}
	srv := httptest.NewServer(idp)
	idp.SetIssuer(srv.URL)
	t.Cleanup(srv.Close)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: srv.URL, ClientID: "console-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
	}, key)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return auth
}

func (f *oidcFlow) do(subject, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if subject != "" {
		r = r.WithContext(withOIDCHuman(r.Context(), subject))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *oidcFlow) signInPath() string { return "/model-providers/" + f.row.UID + "/oidc/sign-in" }

// oidcAttempt is a sign-in that has been to the authority and not yet returned.
type oidcAttempt struct {
	authURL string
	q       url.Values
	cookies []*http.Cookie
}

func (f *oidcFlow) begin(t *testing.T, subject string) oidcAttempt {
	t.Helper()
	w := f.do(subject, f.signInPath(), nil)
	if w.Code != http.StatusFound {
		t.Fatalf("sign-in door: status %d body %q; want a 302 to the authority", w.Code, w.Body.String())
	}
	authURL := w.Header().Get("Location")
	q := follow(t, authURL)
	if q.Get("code") == "" {
		t.Fatalf("the authority issued no code: %v", q)
	}
	return oidcAttempt{authURL: authURL, q: q, cookies: w.Result().Cookies()}
}

func (f *oidcFlow) callback(subject string, q url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	return f.do(subject, oidcCallbackPath+"?"+q.Encode(), cookies)
}

func (f *oidcFlow) capture(t *testing.T, subject string) (oidcAttempt, *httptest.ResponseRecorder) {
	t.Helper()
	at := f.begin(t, subject)
	return at, f.callback(subject, at.q, at.cookies)
}

func (f *oidcFlow) stored(t *testing.T, owner string) (adoEntraBlob, bool) {
	t.Helper()
	blob, found, err := f.srv.endpointOIDCBlob(context.Background(), owner, f.row)
	if err != nil {
		t.Fatalf("read the stored sign-in: %v", err)
	}
	return blob, found
}

func (f *oidcFlow) cookie(t *testing.T, at oidcAttempt, name string) string {
	t.Helper()
	want := f.srv.consoleCookieName(name)
	for _, c := range at.cookies {
		if c.Name == want {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie among %v", want, at.cookies)
	return ""
}

func without(cookies []*http.Cookie, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range cookies {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

func (f *oidcFlow) auditText() string {
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	var sb strings.Builder
	for _, ev := range f.audit.rows {
		b, _ := json.Marshal(ev)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (f *oidcFlow) doneRedirect() string {
	return endpointOIDCDonePath + "&provider=" + url.QueryEscape(oidcUID)
}

func (f *oidcFlow) errorRedirect(reason string) string {
	return endpointOIDCErrorPath + reason + "&provider=" + url.QueryEscape(oidcUID)
}

func oidcSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// ── condition 1: state, nonce, PKCE and the redirect URI ────────────────────

func TestEndpointOIDC_AuthorizationRequestShape(t *testing.T) {
	f := newOIDCFlow(t, "")
	a, b := f.begin(t, oidcPersonSub), f.begin(t, oidcPersonSub)
	u, err := url.Parse(a.authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	verifier := f.cookie(t, a, endpointOIDCPKCECookie)
	sum := sha256.Sum256([]byte(verifier))
	for k, want := range map[string]string{
		"response_type": "code", "client_id": f.row.OIDC.ClientID, "redirect_uri": oidcCallbackURL,
		"code_challenge_method": "S256", "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]),
		"nonce": f.cookie(t, a, endpointOIDCNonceCookie),
	} {
		if q.Get(k) != want {
			t.Errorf("authorization request %s = %q, want %q", k, q.Get(k), want)
		}
	}
	for _, sc := range endpointOIDCRequestedScopes(f.row.OIDC) {
		if !slices.Contains(strings.Fields(q.Get("scope")), sc) {
			t.Errorf("scope %q does not carry %q", q.Get("scope"), sc)
		}
	}
	if q.Get("state") == "" || q.Get("state") == q.Get("nonce") || len(q["state"]) != 1 {
		t.Errorf("state = %v nonce = %q", q["state"], q.Get("nonce"))
	}
	ub, _ := url.Parse(b.authURL)
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if q.Get(k) == ub.Query().Get(k) {
			t.Errorf("two attempts share a %s", k)
		}
	}
	// The redirect URI is matched exactly by the authority: a near miss gets no code.
	q.Set("redirect_uri", oidcCallbackURL+"/x")
	u.RawQuery = q.Encode()
	if got := follow(t, u.String()); got.Get("code") != "" {
		t.Errorf("the authority issued a code to a redirect URI that is not the registered one: %v", got)
	}
}

func TestEndpointOIDC_StateIsExactAndSingleUse(t *testing.T) {
	f := newOIDCFlow(t, "")
	stateCookie := f.srv.consoleCookieName(endpointOIDCStateCookie)
	nonceCookie := f.srv.consoleCookieName(endpointOIDCNonceCookie)
	pkceCookie := f.srv.consoleCookieName(endpointOIDCPKCECookie)
	for _, tc := range []struct {
		name string
		edit func(q url.Values, cookies []*http.Cookie) (url.Values, []*http.Cookie)
	}{
		{"a missing state", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) { q.Del("state"); return q, c }},
		{"an extra state", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) {
			q.Add("state", "another")
			return q, c
		}},
		{"a state that is not the one stamped", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) {
			q.Set("state", "tampered")
			return q, c
		}},
		{"no state cookie", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) { return q, without(c, stateCookie) }},
		{"no nonce cookie", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) { return q, without(c, nonceCookie) }},
		{"no verifier cookie", func(q url.Values, c []*http.Cookie) (url.Values, []*http.Cookie) { return q, without(c, pkceCookie) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := f.begin(t, oidcPersonSub)
			q := url.Values{}
			for k, v := range at.q {
				q[k] = slices.Clone(v)
			}
			q, cookies := tc.edit(q, slices.Clone(at.cookies))
			w := f.callback(oidcPersonSub, q, cookies)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonEndpointOIDCCallbackInvalid) {
				t.Fatalf("status %d body %q; want a 400 %s", w.Code, w.Body.String(), reasonEndpointOIDCCallbackInvalid)
			}
			if _, found := f.stored(t, oidcPersonSub); found {
				t.Fatal("a refused callback stored a sign-in")
			}
			// A forged request does not burn the real attempt.
			if w := f.callback(oidcPersonSub, at.q, at.cookies); w.Code != http.StatusFound || w.Header().Get("Location") != f.doneRedirect() {
				t.Fatalf("the genuine callback after a refused one: status %d Location %q", w.Code, w.Header().Get("Location"))
			}
			f.mem.For(oidcPersonSub).Delete(context.Background(), providerSecretName(oidcUID, providerOIDCPart))
		})
	}

	t.Run("a replayed state", func(t *testing.T) {
		at, w := f.capture(t, oidcPersonSub)
		if w.Code != http.StatusFound || w.Header().Get("Location") != f.doneRedirect() {
			t.Fatalf("capture: status %d Location %q", w.Code, w.Header().Get("Location"))
		}
		cleared := 0
		for _, c := range w.Result().Cookies() {
			if c.MaxAge < 0 && (c.Name == stateCookie || c.Name == nonceCookie || c.Name == pkceCookie) {
				cleared++
			}
		}
		if cleared != 3 {
			t.Errorf("%d of the three one-time cookies were cleared on success", cleared)
		}
		before, _ := f.stored(t, oidcPersonSub)
		w = f.callback(oidcPersonSub, at.q, at.cookies)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonEndpointOIDCCallbackInvalid) {
			t.Fatalf("replay: status %d body %q; want a 400", w.Code, w.Body.String())
		}
		if after, _ := f.stored(t, oidcPersonSub); after.RefreshToken != before.RefreshToken {
			t.Error("a replayed callback replaced the stored sign-in")
		}
	})
}

func TestEndpointOIDC_DoorRefusals(t *testing.T) {
	f := newOIDCFlow(t, "")
	if w := f.do("", f.signInPath(), nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonEndpointOIDCNoSession) {
		t.Errorf("no session: %d %q", w.Code, w.Body.String())
	}
	if w := f.do(oidcPersonSub, "/model-providers/"+oidcOther+"/oidc/sign-in", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonEndpointOIDCUnknownRow) {
		t.Errorf("unknown row: %d %q", w.Code, w.Body.String())
	}
	f.site.set(func(b *types.ModelProviders) { b.Providers[0].Disabled = true })
	if w := f.do(oidcPersonSub, f.signInPath(), nil); w.Code != http.StatusNotFound {
		t.Errorf("disabled row: %d", w.Code)
	}
	f.site.set(func(b *types.ModelProviders) { b.Providers[0].Disabled = false })
	f.srv.cfg.EndpointOIDC = nil
	if w := f.do(oidcPersonSub, f.signInPath(), nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonEndpointOIDCUnconfigured) {
		t.Errorf("unconfigured: %d %q", w.Code, w.Body.String())
	}
}

func TestEndpointOIDC_CallbackRefusesAChangedRowAndAnAuthorityError(t *testing.T) {
	f := newOIDCFlow(t, "")
	at := f.begin(t, oidcPersonSub)
	f.site.set(func(b *types.ModelProviders) {
		o := *b.Providers[0].OIDC
		o.ClientID = "another-client"
		b.Providers[0].OIDC = &o
	})
	w := f.callback(oidcPersonSub, at.q, at.cookies)
	if w.Code != http.StatusFound || w.Header().Get("Location") != f.errorRedirect(azureCaptureRowChanged) {
		t.Fatalf("a row changed under the sign-in: %d Location %q", w.Code, w.Header().Get("Location"))
	}
	if _, found := f.stored(t, oidcPersonSub); found {
		t.Fatal("stored a sign-in for a row that changed")
	}

	f.site.set(func(b *types.ModelProviders) { b.Providers[0].OIDC = f.row.OIDC })
	at = f.begin(t, oidcPersonSub)
	q := url.Values{"state": at.q["state"], "error": {"access_denied"}}
	w = f.callback(oidcPersonSub, q, at.cookies)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), endpointOIDCErrorPath) {
		t.Fatalf("an authority error: %d Location %q", w.Code, w.Header().Get("Location"))
	}
	if _, found := f.stored(t, oidcPersonSub); found {
		t.Fatal("stored a sign-in after an authority error")
	}
}

// ── conditions 1 (token checks end to end) and 4: the person binding ───────

func TestEndpointOIDC_IdentityTokenRefusalsEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*entrafake.Server)
	}{
		{"a wrong issuer", func(s *entrafake.Server) { s.SetIssuerClaim("https://other-authority.example") }},
		{"a wrong audience", func(s *entrafake.Server) { s.SetAudienceClaim("another-client") }},
		{"no identity token", func(s *entrafake.Server) { s.SetOmitIDToken(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCFlow(t, "")
			tc.edit(f.fake)
			_, w := f.capture(t, oidcPersonSub)
			if w.Code != http.StatusFound || w.Header().Get("Location") != f.errorRedirect(reasonADOCallbackIdentityBinding) {
				t.Fatalf("status %d Location %q; want the identity-binding refusal", w.Code, w.Header().Get("Location"))
			}
			if _, found := f.stored(t, oidcPersonSub); found {
				t.Fatal("stored a sign-in whose identity token was refused")
			}
			rows := f.audit.find(endpointOIDCCapturedAction)
			if len(rows) != 1 || rows[0].Outcome != "failure" {
				t.Fatalf("audit rows = %+v; want one failure row", rows)
			}
		})
	}
}

func TestEndpointOIDC_PersonBinding(t *testing.T) {
	t.Run("console binding stores the sign-in for the matching person", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		_, w := f.capture(t, oidcPersonSub)
		if w.Header().Get("Location") != f.doneRedirect() {
			t.Fatalf("Location %q", w.Header().Get("Location"))
		}
		blob, found := f.stored(t, oidcPersonSub)
		if !found || blob.Issuer != f.fake.Issuer() || blob.IDPSubject != oidcPersonSub || blob.Subject != oidcPersonSub || blob.TenantID != "" {
			t.Fatalf("stored = %+v found=%v", blob, found)
		}
	})
	t.Run("console binding refuses another issuer", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		f.env.ConsoleIssuer = "https://the-consoles-own-login.example"
		_, w := f.capture(t, oidcPersonSub)
		if w.Header().Get("Location") != f.errorRedirect(reasonEndpointOIDCIssuerMismatch) {
			t.Fatalf("Location %q", w.Header().Get("Location"))
		}
		if _, found := f.stored(t, oidcPersonSub); found {
			t.Fatal("stored")
		}
	})
	t.Run("console binding with no console issuer refuses", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		f.env.ConsoleIssuer = ""
		if _, w := f.capture(t, oidcPersonSub); w.Header().Get("Location") != f.errorRedirect(reasonEndpointOIDCIssuerMismatch) {
			t.Fatalf("Location %q", w.Header().Get("Location"))
		}
	})
	t.Run("a session whose subject is only the token's email is not that person", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		// The fake's default preferred_username and email claim: a mutable claim, never matched.
		session := "fake.person@example.invalid"
		_, w := f.capture(t, session)
		if w.Header().Get("Location") != f.errorRedirect(reasonADOCallbackIdentityBinding) {
			t.Fatalf("Location %q", w.Header().Get("Location"))
		}
		if _, found := f.stored(t, session); found {
			t.Fatal("stored a sign-in on an email match")
		}
	})
	t.Run("none binding stores for any session and records the authority's identity", func(t *testing.T) {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		f.env.ConsoleIssuer = "https://the-consoles-own-login.example"
		_, w := f.capture(t, "someone-else")
		if w.Header().Get("Location") != f.doneRedirect() {
			t.Fatalf("Location %q", w.Header().Get("Location"))
		}
		blob, found := f.stored(t, "someone-else")
		if !found || blob.IDPSubject != oidcPersonSub || blob.Issuer != f.fake.Issuer() || blob.Subject != "someone-else" {
			t.Fatalf("stored = %+v found=%v", blob, found)
		}
		rows := f.audit.find(endpointOIDCCapturedAction)
		if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"binding":"none"`) {
			t.Fatalf("audit rows = %+v; want the none binding recorded", rows)
		}
		if !f.row.OIDC.Unbound() {
			t.Error("a none binding does not report as not bound")
		}
	})
}

// ── conditions 3 and 5: the full flow, no token anywhere, no scope sent ─────

func TestEndpointOIDC_FullFlowLeaksNoToken(t *testing.T) {
	f := newOIDCFlow(t, "")
	subject := oidcPersonSub
	at, w := f.capture(t, subject)
	if w.Code != http.StatusFound || w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("capture: status %d Location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	before, found := f.stored(t, subject)
	want := []string{entraOpenIDScope, entraOfflineAccessScope, "api.read"}
	if !found || before.RefreshToken == "" || !slices.Equal(oidcSorted(before.Scopes), oidcSorted(want)) {
		t.Fatalf("stored = %+v found=%v; want the requested set %v (RFC 6749 section 5.1)", before, found, want)
	}
	if raw, _ := json.Marshal(before); strings.Contains(string(raw), "access_token") {
		t.Errorf("the stored sign-in carries an access token: %s", raw)
	}

	// Only the person's own namespace holds it; the operator's holds no sign-in.
	name := providerSecretName(oidcUID, providerOIDCPart)
	if ops, _ := f.srv.cfg.Secrets.List(context.Background()); slices.Contains(ops, name) {
		t.Errorf("the operator namespace holds %q", name)
	}
	if own, _ := f.srv.cfg.Secrets.For(subject).List(context.Background()); !slices.Contains(own, name) {
		t.Errorf("the person's namespace %v does not hold %q", own, name)
	}
	if !reservedSecret(name) || !sinkReservedSecret(name) || !nameSinkReservedSecret(name) {
		t.Errorf("%q is not refused at every sink", name)
	}

	access, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), subject, f.row)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if access.AccessToken == "" || !slices.Equal(oidcSorted(access.Scopes), oidcSorted(want)) {
		t.Fatalf("access = %+v", access)
	}
	if got := f.fake.RefreshRequestScopes(); !slices.Equal(got, []string{""}) {
		t.Errorf("the refresh sent scope %q; an endpoint sign-in sends none", got)
	}
	after, _ := f.stored(t, subject)
	if after.RefreshToken == before.RefreshToken || after.RenewedAt.IsZero() {
		t.Errorf("the rotation was not persisted: before %q after %q", before.RefreshToken, after.RefreshToken)
	}
	if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), subject, f.row); err != nil {
		t.Fatalf("second redeem, from the rotated token: %v", err)
	}

	// Failure paths write to the same logs and audit: a replay, a refused identity token, a changed row.
	f.callback(subject, at.q, at.cookies)
	f.fake.SetIssuerClaim("https://other-authority.example")
	f.capture(t, subject)
	f.fake.SetIssuerClaim("")

	secrets := append(f.wire.all(), oidcClientSecret, at.q.Get("code"), f.cookie(t, at, endpointOIDCPKCECookie))
	if len(f.wire.all()) < 5 {
		t.Fatalf("the proxy saw %d token values; want at least five", len(f.wire.all()))
	}
	haystack := map[string]string{"log": f.logs.String(), "audit": f.auditText(), "response": w.Body.String() + w.Header().Get("Location")}
	for where, text := range haystack {
		for _, sec := range secrets {
			if sec != "" && strings.Contains(text, sec) {
				t.Errorf("the %s carries a secret value (%d bytes)", where, len(sec))
			}
		}
	}
	rows := f.audit.find(endpointOIDCCapturedAction)
	if len(rows) < 2 || rows[0].Outcome != "success" || rows[0].Actor != subject || rows[0].Target != name {
		t.Fatalf("audit rows = %+v; want a success row attributed to the caller, targeting the sealed name", rows)
	}
}

// ── condition 6: a revoked or offboarded person's tokens stop being used ────

func TestEndpointOIDC_ErasedAndPurgedSignInsAreGone(t *testing.T) {
	f := newOIDCFlow(t, types.OIDCBindingNone)
	f.capture(t, oidcPersonSub)
	f.capture(t, "second-person")
	if _, found := f.stored(t, oidcPersonSub); !found {
		t.Fatal("setup: nothing stored")
	}
	// The offboarding hook is whole-namespace erasure.
	if _, err := secretstore.EraseOwner(context.Background(), f.srv.cfg.Secrets, oidcPersonSub); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if _, found := f.stored(t, oidcPersonSub); found {
		t.Fatal("the stored sign-in survived the erasure")
	}
	if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Fatalf("redeem after erasure: %v; want ErrADOEntraNotCaptured", err)
	}
	if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
		t.Errorf("an erased person's refresh token was still presented %d times", len(got))
	}
	if _, found := f.stored(t, "second-person"); !found {
		t.Error("erasing one person removed another's sign-in")
	}

	// Rule 8: a changed address purges the part for everyone.
	f2 := newOIDCFlow(t, types.OIDCBindingNone)
	f2.capture(t, "person-a")
	f2.capture(t, "person-b")
	next := f2.row
	o := *next.OIDC
	o.TokenURL = "https://other-authority.example/token"
	next.OIDC = &o
	if _, err := f2.srv.purgeProviderCredentials(context.Background(),
		&types.ModelProviders{Providers: []types.ModelProvider{f2.row}}, &types.ModelProviders{Providers: []types.ModelProvider{next}}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, who := range []string{"person-a", "person-b"} {
		if _, found := f2.stored(t, who); found {
			t.Errorf("%s's sign-in survived a changed token URL", who)
		}
		if _, err := f2.srv.RedeemEndpointOIDCAccess(context.Background(), who, f2.row); !errors.Is(err, ErrADOEntraNotCaptured) {
			t.Errorf("%s: redeem after the purge: %v", who, err)
		}
	}
}

// ── condition 7: rotation, a lost write, reuse detection ────────────────────

// oidcLossySecrets is a secret store whose owner views can lose a write or lose a race for one, and which keeps
// a row revision so the guarded Put the rotation uses is exercised.
type oidcLossySecrets struct {
	secretstore.Store
	failPut atomic.Bool // every Put to an owner view fails
	racePut atomic.Bool // a concurrent writer lands just before each guarded Put
}

func (l *oidcLossySecrets) For(owner string) secretstore.Store {
	return &oidcLossyView{Store: l.Store.For(owner), l: l}
}

type oidcLossyView struct {
	secretstore.Store
	l *oidcLossySecrets
}

func (v *oidcLossyView) Revision(ctx context.Context, name string) (string, error) {
	raw, err := v.Store.Get(ctx, name)
	if errors.Is(err, secretstore.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (v *oidcLossyView) Put(ctx context.Context, name string, val []byte) error {
	if v.l.failPut.Load() {
		return errors.New("injected: the store lost the write")
	}
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		if v.l.racePut.Load() {
			var b adoEntraBlob
			if raw, err := v.Store.Get(ctx, name); err == nil && json.Unmarshal(raw, &b) == nil {
				b.RenewedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				out, _ := json.Marshal(b)
				_ = v.Store.Put(ctx, name, out)
			}
		}
		if cur, err := v.Revision(ctx, name); err != nil || cur != rev {
			return secretstore.ErrRevisionChanged
		}
	}
	return v.Store.Put(ctx, name, val)
}

func (f *oidcFlow) lossy() *oidcLossySecrets {
	l := &oidcLossySecrets{Store: f.mem}
	f.srv.cfg.Secrets = l
	return l
}

func (f *oidcFlow) assertEveryRefreshTokenDead(t *testing.T) {
	t.Helper()
	n := 0
	for _, tok := range f.wire.all() {
		if !strings.HasPrefix(tok, "fake-entra-refresh-") {
			continue
		}
		n++
		if live, _ := f.fake.RefreshTokenState(tok); live {
			t.Errorf("a refresh token is still live after reuse was detected")
		}
	}
	if n < 2 {
		t.Fatalf("saw %d refresh tokens; want a spent one and its replacement", n)
	}
}

func TestEndpointOIDC_ALostRotationWriteFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lose  func(*oidcLossySecrets)
		note  string
		reset func(*oidcLossySecrets)
	}{
		{"the store drops the write", func(l *oidcLossySecrets) { l.failPut.Store(true) }, "persisting the rotated", func(l *oidcLossySecrets) { l.failPut.Store(false) }},
		{"a concurrent writer wins the compare-and-set", func(l *oidcLossySecrets) { l.racePut.Store(true) }, "changed or was erased while it was being renewed", func(l *oidcLossySecrets) { l.racePut.Store(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCFlow(t, "")
			f.fake.SetReuseDetection(true)
			l := f.lossy()
			f.capture(t, oidcPersonSub)
			before, _ := f.stored(t, oidcPersonSub)

			tc.lose(l)
			access, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row)
			if err != nil || access.AccessToken == "" {
				t.Fatalf("the redemption that lost its write: %+v, %v; it serves its caller from memory", access, err)
			}
			if !strings.Contains(f.logs.String(), tc.note) {
				t.Errorf("the lost write was not logged (%q)", tc.note)
			}
			if got, _ := f.stored(t, oidcPersonSub); got.RefreshToken != before.RefreshToken {
				t.Fatal("the lost write changed the stored refresh token")
			}
			tc.reset(l)

			// The stored token is spent: the authority revokes the family and the sign-in is gone.
			if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row); !errors.Is(err, ErrADOEntraDeadCredential) {
				t.Fatalf("the redemption after a lost write: %v; want ErrADOEntraDeadCredential", err)
			}
			if _, found := f.stored(t, oidcPersonSub); found {
				t.Error("a dead sign-in was kept")
			}
			if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row); !errors.Is(err, ErrADOEntraNotCaptured) {
				t.Errorf("after the dead sign-in was deleted: %v; want ErrADOEntraNotCaptured", err)
			}
			f.assertEveryRefreshTokenDead(t)
		})
	}
}

// ── the state cookie is signed, bound to the session, and cannot be used to lock anyone out ─────────────────

func withCookieValue(cookies []*http.Cookie, name, value string) []*http.Cookie {
	out := without(cookies, name)
	return append(out, &http.Cookie{Name: name, Value: value})
}

func oidcSpentLen() int {
	endpointOIDCSpent.Lock()
	defer endpointOIDCSpent.Unlock()
	return len(endpointOIDCSpent.until)
}

func TestEndpointOIDC_ForgedCallbacksCannotFillTheSpentSet(t *testing.T) {
	f := newOIDCFlow(t, "")
	stateCookie := f.srv.consoleCookieName(endpointOIDCStateCookie)
	base := oidcSpentLen()
	for i := 0; i < endpointOIDCSpentMax+1; i++ {
		state := fmt.Sprintf("forged-state-%d", i)
		raw, _ := json.Marshal(endpointOIDCStamp{
			State: state, RowUID: oidcUID, Digest: "d", Person: endpointOIDCPersonDigest(oidcPersonSub), Expires: adoTestNow.Add(time.Hour).Unix(),
		})
		forged := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString([]byte("not a signature"))
		cookies := []*http.Cookie{
			{Name: stateCookie, Value: forged},
			{Name: f.srv.consoleCookieName(endpointOIDCNonceCookie), Value: "n"},
			{Name: f.srv.consoleCookieName(endpointOIDCPKCECookie), Value: "v"},
		}
		if w := f.callback(oidcPersonSub, url.Values{"state": {state}, "code": {"c"}}, cookies); w.Code != http.StatusBadRequest {
			t.Fatalf("forged callback %d: status %d; want 400", i, w.Code)
		}
	}
	if got := oidcSpentLen(); got != base {
		t.Fatalf("forged callbacks left %d entries in the spent set (was %d)", got, base)
	}
	if _, w := f.capture(t, oidcPersonSub); w.Code != http.StatusFound || w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("a genuine sign-in after the forgeries: status %d Location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestEndpointOIDC_SpentSetDropsItsOldestEntryWhenFull(t *testing.T) {
	endpointOIDCSpent.Lock()
	saved := endpointOIDCSpent.until
	endpointOIDCSpent.until = map[[sha256.Size]byte]time.Time{}
	endpointOIDCSpent.Unlock()
	t.Cleanup(func() {
		endpointOIDCSpent.Lock()
		endpointOIDCSpent.until = saved
		endpointOIDCSpent.Unlock()
	})
	now := adoTestNow
	for i := 0; i < endpointOIDCSpentMax; i++ {
		if !spendEndpointOIDCState(fmt.Sprintf("s%d", i), now.Add(time.Duration(i+1)*time.Second), now) {
			t.Fatalf("state %d refused while the set was filling", i)
		}
	}
	if !spendEndpointOIDCState("one-more", now.Add(time.Hour), now) {
		t.Fatal("a full set refused a fresh state: whoever fills it could lock everyone out")
	}
	if got := oidcSpentLen(); got != endpointOIDCSpentMax {
		t.Errorf("the set holds %d entries; want it capped at %d", got, endpointOIDCSpentMax)
	}
	if spendEndpointOIDCState("one-more", now.Add(time.Hour), now) {
		t.Error("a state spent twice")
	}
	endpointOIDCSpent.Lock()
	_, oldestKept := endpointOIDCSpent.until[sha256.Sum256([]byte("s0"))]
	_, newerKept := endpointOIDCSpent.until[sha256.Sum256([]byte("s1"))]
	endpointOIDCSpent.Unlock()
	if oldestKept || !newerKept {
		t.Errorf("eviction kept the oldest (%v) or dropped a newer one (kept=%v)", oldestKept, newerKept)
	}
	// An entry is forgotten once its attempt has expired.
	if !spendEndpointOIDCState("s1", now.Add(time.Hour), now.Add(2*time.Hour)) {
		t.Error("an expired entry still blocks its state")
	}
}

func TestEndpointOIDC_StampIsSignedBoundToTheSessionAndExpires(t *testing.T) {
	stateCookie := func(f *oidcFlow) string { return f.srv.consoleCookieName(endpointOIDCStateCookie) }
	t.Run("cookies from one person's attempt are refused in another person's session", func(t *testing.T) {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		at := f.begin(t, "person-x")
		w := f.callback("person-y", at.q, at.cookies)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonEndpointOIDCCallbackInvalid) {
			t.Fatalf("status %d body %q; want a 400 %s", w.Code, w.Body.String(), reasonEndpointOIDCCallbackInvalid)
		}
		for _, who := range []string{"person-x", "person-y"} {
			if _, found := f.stored(t, who); found {
				t.Errorf("%s holds a sign-in after the planted callback", who)
			}
		}
		if w := f.callback("person-x", at.q, at.cookies); w.Header().Get("Location") != f.doneRedirect() {
			t.Errorf("the owner's own callback after the refused one: %d %q", w.Code, w.Header().Get("Location"))
		}
	})
	t.Run("a stamp with its body altered is refused", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		at := f.begin(t, oidcPersonSub)
		body, sig, _ := strings.Cut(f.cookie(t, at, endpointOIDCStateCookie), ".")
		raw, _ := base64.RawURLEncoding.DecodeString(body)
		var st endpointOIDCStamp
		_ = json.Unmarshal(raw, &st)
		st.Digest = "another-digest"
		altered, _ := json.Marshal(st)
		forged := base64.RawURLEncoding.EncodeToString(altered) + "." + sig
		w := f.callback(oidcPersonSub, at.q, withCookieValue(at.cookies, stateCookie(f), forged))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d; want 400", w.Code)
		}
	})
	t.Run("a stamp signed under another key is refused", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		at := f.begin(t, oidcPersonSub)
		other := newOIDCStampAuth(t, bytes.Repeat([]byte{3}, 32))
		body, _, _ := strings.Cut(f.cookie(t, at, endpointOIDCStateCookie), ".")
		raw, _ := base64.RawURLEncoding.DecodeString(body)
		mac := other.PurposeMAC(endpointOIDCStampPurpose, raw)
		forged := body + "." + base64.RawURLEncoding.EncodeToString(mac)
		if w := f.callback(oidcPersonSub, at.q, withCookieValue(at.cookies, stateCookie(f), forged)); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d; want 400", w.Code)
		}
	})
	t.Run("two replicas sharing the key accept each other's stamp", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		at := f.begin(t, oidcPersonSub)
		f.srv.cfg.OIDC = newOIDCStampAuth(t, bytes.Repeat([]byte{9}, 32))
		if w := f.callback(oidcPersonSub, at.q, at.cookies); w.Header().Get("Location") != f.doneRedirect() {
			t.Fatalf("status %d Location %q", w.Code, w.Header().Get("Location"))
		}
	})
	t.Run("an expired attempt is refused", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		at := f.begin(t, oidcPersonSub)
		f.srv.cfg.Now = func() time.Time { return adoTestNow.Add(11 * time.Minute) }
		if w := f.callback(oidcPersonSub, at.q, at.cookies); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d; want 400", w.Code)
		}
	})
	t.Run("a process with no console key offers no sign-in", func(t *testing.T) {
		f := newOIDCFlow(t, "")
		f.srv.cfg.OIDC = nil
		if w := f.do(oidcPersonSub, f.signInPath(), nil); w.Code != http.StatusNotFound {
			t.Fatalf("status %d; want 404", w.Code)
		}
	})
}

// ── discovery documents may not name an address a gateway can never be ──────────────────────────────────────

func TestEndpointOIDC_DiscoveredURLsRefuseUnsafeIPLiterals(t *testing.T) {
	for _, tc := range []struct {
		url       string
		allowTest bool
		want      bool
	}{
		{"https://idp.example/oauth/token", false, true},
		{"https://10.1.2.3/oauth/token", false, true}, // an internal authority is expected on RFC 1918
		{"https://169.254.169.254/latest", false, false},
		{"https://127.0.0.1/oauth/token", false, false},
		{"https://[::1]/oauth/token", false, false},
		{"https://0.0.0.0/oauth/token", false, false},
		{"https://user:pw@idp.example/token", false, false},
		{"http://idp.example/token", false, false},
		{"https://idp.example/token#frag", false, false},
		{"", false, false},
		{"http://127.0.0.1:8080/token", true, true},
	} {
		if got := endpointOIDCURLUsable(tc.url, tc.allowTest); got != tc.want {
			t.Errorf("endpointOIDCURLUsable(%q, %v) = %v, want %v", tc.url, tc.allowTest, got, tc.want)
		}
	}
	good := endpointOIDCDiscovery{authorizeURL: "https://idp.example/a", tokenURL: "https://idp.example/t", revocationURL: "https://idp.example/r"}
	const metadata = "https://169.254.169.254/latest"
	for name, edit := range map[string]func(*endpointOIDCDiscovery, *string){
		"authorization_endpoint": func(d *endpointOIDCDiscovery, _ *string) { d.authorizeURL = metadata },
		"token_endpoint":         func(d *endpointOIDCDiscovery, _ *string) { d.tokenURL = metadata },
		"jwks_uri":               func(_ *endpointOIDCDiscovery, j *string) { *j = metadata },
	} {
		d, jwks := good, "https://idp.example/keys"
		edit(&d, &jwks)
		if err := checkEndpointOIDCURLs(&d, jwks, false); err == nil {
			t.Errorf("a discovery document with a metadata-address %s was accepted", name)
		}
	}
	d := good
	d.revocationURL = metadata
	if err := checkEndpointOIDCURLs(&d, "https://idp.example/keys", false); err != nil || d.revocationURL != "" {
		t.Errorf("a metadata-address revocation_endpoint: err=%v kept=%q; want it dropped and the rest accepted", err, d.revocationURL)
	}
	d = good
	if err := checkEndpointOIDCURLs(&d, "https://idp.example/keys", false); err != nil || d.revocationURL == "" {
		t.Errorf("a clean document: err=%v revocation=%q", err, d.revocationURL)
	}
}

// A second sign-in leaves the stored credential's AddedAt where it was (a replaced value keeps it), so what tells a
// console that a sign-in it started has landed is SignedInAt: it moves on every capture and the status reports it.
func TestEndpointOIDC_SignedInAtMovesOnEverySignIn(t *testing.T) {
	f := newOIDCFlow(t, "")
	grade := func() SetupProviderAccess {
		var row SetupProviderAccess
		f.srv.gradeProviderEndpointOIDC(context.Background(), &row, f.row, oidcPersonSub)
		return row
	}
	if got := grade(); got.SignedInAt != nil || got.State != modelAccessNotConfigured {
		t.Fatalf("before any sign-in: %+v; want not_configured and no signed_in_at", got)
	}
	if _, w := f.capture(t, oidcPersonSub); w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("first sign-in: Location %q", w.Header().Get("Location"))
	}
	first := grade()
	if first.State != modelAccessLive || first.SignedInAt == nil || !first.SignedInAt.Equal(adoTestNow) {
		t.Fatalf("after the first sign-in: %+v; want live, signed in at %s", first, adoTestNow)
	}
	f.srv.cfg.Now = func() time.Time { return adoTestNow.Add(time.Hour) }
	if _, w := f.capture(t, oidcPersonSub); w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("second sign-in: Location %q", w.Header().Get("Location"))
	}
	second := grade()
	if second.SignedInAt == nil || !second.SignedInAt.Equal(adoTestNow.Add(time.Hour)) {
		t.Fatalf("after the second sign-in: %+v; want signed in an hour later", second)
	}
}
