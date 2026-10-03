// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The two resources the fixture's tenant holds a delegated permission for.
const (
	azFoundryScope   = "https://ai.azure.com/user_impersonation"
	azCognitiveScope = "https://cognitiveservices.azure.com/user_impersonation"
)

// Provider uids the fixture's site config holds.
const (
	azUIDAnthropic = "11111111-aaaa-4aaa-8aaa-000000000001" // azure_foundry, route anthropic
	azUIDOpenAI    = "11111111-aaaa-4aaa-8aaa-000000000002" // azure_foundry, route openai_v1
	azUIDOther     = "11111111-aaaa-4aaa-8aaa-000000000003" // anthropic_api_key
)

// azureSiteStore serves a mutable site config, so a row can change, move or
// vanish under a live sign-in. Everything else is the nil embedded Store.
type azureSiteStore struct {
	store.Store
	mu sync.Mutex
	sc types.SiteConfig
}

func (s *azureSiteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sc, nil
}

func (s *azureSiteStore) set(f func(*types.ModelProviders)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := &types.ModelProviders{Providers: slices.Clone(s.sc.ModelProviders.Providers)}
	f(block)
	s.sc.ModelProviders = block
}

func azureRows() []types.ModelProvider {
	return []types.ModelProvider{
		{ID: "foundry-claude", UID: azUIDAnthropic, Kind: types.ModelProviderAzureFoundry,
			Azure: &types.AzureSettings{Endpoint: "https://res.services.ai.azure.com", Route: types.AzureRouteAnthropic}},
		{ID: "foundry-openai", UID: azUIDOpenAI, Kind: types.ModelProviderAzureFoundry,
			Azure: &types.AzureSettings{Endpoint: "https://res.openai.azure.com", Route: types.AzureRouteOpenAIV1}},
		{ID: "plain", UID: azUIDOther, Kind: types.ModelProviderAnthropicAPIKey},
	}
}

// azureFixture is one wired-up Azure Foundry capture: a fake Entra tenant that
// holds the two resources' permissions, a server whose by-uid source points at
// it, and a site config holding the rows above.
type azureFixture struct {
	srv   *Server
	audit *memAudit
	fake  *entrafake.Server
	site  *azureSiteStore
	app   ADOEntraConfig
}

func newAzureFixture(t *testing.T) *azureFixture {
	t.Helper()
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	fake.SetRedirectURI(adoCallbackURL)
	fake.SetConsentedScopes(azFoundryScope, azCognitiveScope)

	f := &azureFixture{fake: fake, audit: &memAudit{}}
	f.site = &azureSiteStore{sc: types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: azureRows()}}}
	f.app = ADOEntraConfig{
		TenantID:           fake.TenantID(),
		ClientID:           fake.ClientID(),
		RedirectURL:        adoCallbackURL,
		LoginClientID:      fake.ClientID(),
		LoginTenantID:      fake.TenantID(),
		AuthorityOverride:  fake.URL(),
		AllowTestEndpoints: true,
	}
	f.srv = &Server{cfg: Config{
		Store:        f.site,
		Secrets:      &memSecrets{m: map[string][]byte{}},
		MaskRegistry: secretmask.NewRegistry(),
		Now:          func() time.Time { return adoTestNow },
		Audit:        f.audit,
		AzureFoundryEntra: func(_ context.Context, uid string) (ADOEntraConfig, bool, error) {
			sc, _ := f.site.GetSiteConfig(context.Background())
			for _, p := range modelProviderRows(sc) {
				if p.UID == uid && p.Kind == types.ModelProviderAzureFoundry {
					cfg := f.app
					cfg.RowID = uid
					return cfg, true, nil
				}
			}
			return ADOEntraConfig{}, false, nil
		},
	}}
	return f
}

// start drives the start door for uid.
func (f *azureFixture) start(subject, uid string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, azureFoundrySignInRoute+"?uid="+url.QueryEscape(uid), nil)
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	f.srv.handleAzureFoundrySignIn(w, r)
	return w
}

// callback drives the SHARED callback door (handleADOCallback), which is where
// the redirect URI points.
func (f *azureFixture) callback(subject string, q url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/callback?"+q.Encode(), nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	f.srv.handleADOCallback(w, r)
	return w
}

// begin starts a sign-in for uid and walks the authority, returning what the
// callback needs.
func (f *azureFixture) begin(t *testing.T, subject, uid string) (url.Values, []*http.Cookie, string) {
	t.Helper()
	w := f.start(subject, uid)
	if w.Code != http.StatusFound {
		t.Fatalf("start: status %d body %q; want a 302 to the authority", w.Code, w.Body.String())
	}
	authURL := w.Header().Get("Location")
	q := follow(t, authURL)
	if q.Get("code") == "" {
		t.Fatalf("the authority issued no code: %v", q)
	}
	return q, w.Result().Cookies(), authURL
}

// capture runs a whole sign-in for uid.
func (f *azureFixture) capture(t *testing.T, subject, uid string) *httptest.ResponseRecorder {
	t.Helper()
	q, cookies, _ := f.begin(t, subject, uid)
	return f.callback(subject, q, cookies)
}

func (f *azureFixture) stored(t *testing.T, owner, uid string) (adoEntraBlob, bool) {
	t.Helper()
	ec, err := azureFoundryCapture(uid, azureFoundryAudience)
	if err != nil {
		t.Fatalf("capture for %s: %v", uid, err)
	}
	blob, found, err := f.srv.readEntraBlob(context.Background(), owner, ec)
	if err != nil {
		t.Fatalf("read the stored sign-in: %v", err)
	}
	return blob, found
}

func (f *azureFixture) storedNames(t *testing.T, owner string) []string {
	t.Helper()
	names, err := f.srv.cfg.Secrets.For(owner).List(context.Background())
	if err != nil {
		t.Fatalf("list %s's secrets: %v", owner, err)
	}
	return names
}

func azureErrorRedirect(reason string) string { return azureFoundrySignInErrorPath + reason }

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, c := range cookies {
		if strings.HasSuffix(c.Name, name) {
			return c
		}
	}
	t.Fatalf("no cookie named %q in %v", name, cookies)
	return nil
}

// withCookie returns cookies with name's value replaced.
func withCookie(cookies []*http.Cookie, name, value string) []*http.Cookie {
	out := make([]*http.Cookie, len(cookies))
	for i, c := range cookies {
		cp := *c
		if strings.HasSuffix(c.Name, name) {
			cp.Value = value
		}
		out[i] = &cp
	}
	return out
}

// ── scope policy ────────────────────────────────────────────────────────────

// TestEntraScopePolicy pins the per-kind policy: Azure DevOps refuses every
// `.default` and keeps its exact-literal intersect, byte for byte; azure_foundry
// admits exactly its route's audience plus the identity scopes and refuses every
// other `.default`.
func TestEntraScopePolicy(t *testing.T) {
	ado := adoCapture(ADOEntraConfig{RowID: "ado-row-1", Scopes: []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code"}})
	foundry, err := azureFoundryCapture(azUIDAnthropic, azureFoundryAudience)
	if err != nil {
		t.Fatal(err)
	}
	cognitive, err := azureFoundryCapture(azUIDOpenAI, azureCognitiveServicesAudience)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		capture entraCapture
		scopes  []string
		ok      bool
	}{
		{"ado refuses a resource .default", ado, []string{"499b84ac-1321-427f-aa17-267ca6975798/.default"}, false},
		{"ado refuses the foundry audience", ado, []string{azureFoundryAudience}, false},
		{"ado refuses a bare .default", ado, []string{".default"}, false},
		{"ado admits a literal", ado, []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code"}, true},
		{"foundry admits its audience with the identity scopes", foundry, []string{azureFoundryAudience, "offline_access", "openid"}, true},
		{"foundry admits its audience alone", foundry, []string{azureFoundryAudience}, true},
		{"foundry refuses the other route's audience", foundry, []string{azureCognitiveServicesAudience}, false},
		{"foundry refuses any other .default", foundry, []string{"https://example.invalid/.default"}, false},
		{"foundry refuses the ado resource .default", foundry, []string{"499b84ac-1321-427f-aa17-267ca6975798/.default"}, false},
		{"foundry refuses a bare .default", foundry, []string{".default"}, false},
		{"foundry refuses a non-default scope", foundry, []string{azFoundryScope}, false},
		{"foundry refuses its audience beside another", foundry, []string{azureFoundryAudience, azureCognitiveServicesAudience}, false},
		{"foundry refuses an empty request", foundry, nil, false},
		{"cognitive admits its audience", cognitive, []string{azureCognitiveServicesAudience, "openid"}, true},
		{"cognitive refuses the foundry audience", cognitive, []string{azureFoundryAudience}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.capture.checkRequested(tc.scopes); (err == nil) != tc.ok {
				t.Fatalf("checkRequested(%v) = %v; want ok=%v", tc.scopes, err, tc.ok)
			}
		})
	}
}

// TestADOEntraConfigStillRefusesDotDefault: validation, the sign-in query and a
// redemption each still refuse an Azure DevOps `.default`.
func TestADOEntraConfigStillRefusesDotDefault(t *testing.T) {
	f := newADOFixture(t)
	bad := f.cfg
	bad.Scopes = append(slices.Clone(f.cfg.Scopes), "499b84ac-1321-427f-aa17-267ca6975798/.default")
	if err := bad.validate(); err == nil {
		t.Error("validate accepted an Azure DevOps ceiling carrying a .default")
	}
	if _, err := adoRequestedScopes(url.Values{"scopes": {"499b84ac-1321-427f-aa17-267ca6975798/.default"}}, bad.Scopes); err == nil {
		t.Error("the sign-in query accepted a .default")
	}
	if _, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, "someone", []string{"499b84ac-1321-427f-aa17-267ca6975798/.default"}); err == nil {
		t.Error("a redemption accepted a .default")
	}
}

func TestAzureCapturedScopes(t *testing.T) {
	ec, err := azureFoundryCapture(azUIDAnthropic, azureFoundryAudience)
	if err != nil {
		t.Fatal(err)
	}
	got := ec.capturedScopes("openid offline_access " + azFoundryScope + " " + azCognitiveScope + " https://ai.azure.com/.default " + azFoundryScope)
	if !slices.Equal(got, []string{azFoundryScope}) {
		t.Errorf("capturedScopes = %v; want only the entry under the resource, once, never the literal", got)
	}
	if got := ec.capturedScopes("openid offline_access " + azCognitiveScope); len(got) != 0 {
		t.Errorf("an answer naming only another resource stored %v", got)
	}
	if got := ec.capturedScopes("https://ai.azure.com/"); len(got) != 0 {
		t.Errorf("the bare resource stored %v", got)
	}
}

func TestAzureFoundryCaptureRefusesAnOpenAudienceAndAnUnusableUID(t *testing.T) {
	for _, aud := range []string{"", "https://example.invalid/.default", azureFoundryAudience + " ", "https://ai.azure.com", azFoundryScope} {
		if _, err := azureFoundryCapture(azUIDAnthropic, aud); err == nil {
			t.Errorf("audience %q was accepted", aud)
		}
	}
	for _, uid := range []string{"", "-leading", "row/../other", "a b", "x-oauth", strings.Repeat("a", 65)} {
		if _, err := azureFoundryCapture(uid, azureFoundryAudience); err == nil {
			t.Errorf("uid %q was accepted", uid)
		}
	}
}

func TestAzureAudienceTable(t *testing.T) {
	if a, ok := azureAudienceForRoute(types.AzureRouteAnthropic); !ok || a != azureFoundryAudience {
		t.Errorf("anthropic route -> %q, %v", a, ok)
	}
	if a, ok := azureAudienceForRoute(types.AzureRouteOpenAIV1); !ok || a != azureCognitiveServicesAudience {
		t.Errorf("openai_v1 route -> %q, %v", a, ok)
	}
	if _, ok := azureAudienceForRoute("responses"); ok {
		t.Error("an unknown route has an audience")
	}
}

// ── the login door is never widened ─────────────────────────────────────────

// TestLoginScopesIgnoreAzureRows: an Azure DevOps row plus an azure_foundry row
// yields exactly today's login scopes, and an azure_foundry row alone yields nil.
// Entra refuses a login naming two resources (AADSTS700022), so widening it
// would break every sign-in.
func TestLoginScopesIgnoreAzureRows(t *testing.T) {
	f := newADOFixture(t)
	site := &azureSiteStore{sc: types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: azureRows()}}}
	f.srv.cfg.Store = site
	f.srv.cfg.AzureFoundryEntra = func(_ context.Context, uid string) (ADOEntraConfig, bool, error) {
		cfg := f.cfg
		cfg.RowID = uid
		return cfg, true, nil
	}

	want := append(slices.Clone(f.cfg.Scopes), entraOfflineAccessScope)
	if got := f.srv.LoginScopes(context.Background()); !slices.Equal(got, want) {
		t.Fatalf("LoginScopes with an Azure DevOps row and Azure rows = %v; want exactly today's %v", got, want)
	}
	for _, sc := range f.srv.LoginScopes(context.Background()) {
		if strings.Contains(sc, "azure.com") {
			t.Fatalf("the login request carries an Azure audience: %q", sc)
		}
	}

	f.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return ADOEntraConfig{}, false, nil }
	if got := f.srv.LoginScopes(context.Background()); got != nil {
		t.Fatalf("LoginScopes with an azure_foundry row alone = %v; want nil", got)
	}
}

// ── start door ──────────────────────────────────────────────────────────────

func TestAzureFoundrySignIn_RequestShape(t *testing.T) {
	f := newAzureFixture(t)
	for _, tc := range []struct{ uid, audience string }{
		{azUIDAnthropic, azureFoundryAudience},
		{azUIDOpenAI, azureCognitiveServicesAudience},
	} {
		w := f.start(f.fake.Subject(), tc.uid)
		if w.Code != http.StatusFound {
			t.Fatalf("%s: status %d body %q", tc.uid, w.Code, w.Body.String())
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := loc.Query()
		if got, want := q.Get("scope"), tc.audience+" offline_access openid"; got != want {
			t.Errorf("%s: scope = %q; want exactly %q", tc.uid, got, want)
		}
		if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != adoCallbackURL {
			t.Errorf("%s: authorization request shape: %v", tc.uid, q)
		}
		stamp, ok := decodeAzureFoundryState(cookieNamed(t, w.Result().Cookies(), azureStateCookieName).Value)
		if !ok || stamp.RowUID != tc.uid || stamp.Audience != tc.audience || stamp.State != q.Get("state") || stamp.Digest == "" {
			t.Errorf("%s: state cookie stamp = %+v (state in URL %q)", tc.uid, stamp, q.Get("state"))
		}
		for _, c := range w.Result().Cookies() {
			if !c.HttpOnly {
				t.Errorf("cookie %s is not HttpOnly", c.Name)
			}
		}
	}
}

func TestAzureFoundrySignIn_RefusesAUIDThatIsNotAnAzureFoundryRow(t *testing.T) {
	f := newAzureFixture(t)
	for name, uid := range map[string]string{
		"another kind": azUIDOther, "unknown": "11111111-aaaa-4aaa-8aaa-0000000000ff", "empty": "",
	} {
		w := f.start(f.fake.Subject(), uid)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonAzureSignInUnknownRow) {
			t.Errorf("%s: status %d body %q; want a 404 %s", name, w.Code, w.Body.String(), reasonAzureSignInUnknownRow)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: a refused start set cookies", name)
		}
	}
}

func TestAzureFoundrySignIn_RefusesNoSessionAndNoConsoleLogin(t *testing.T) {
	f := newAzureFixture(t)
	if w := f.start("", azUIDAnthropic); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonAzureSignInNoSession) {
		t.Errorf("no session: status %d body %q", w.Code, w.Body.String())
	}
	f.app.LoginClientID, f.app.LoginTenantID = "", ""
	if w := f.start(f.fake.Subject(), azUIDAnthropic); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonAzureSignInUnconfigured) {
		t.Errorf("no console login: status %d body %q", w.Code, w.Body.String())
	}
	f.srv.cfg.AzureFoundryEntra = nil
	if w := f.start(f.fake.Subject(), azUIDAnthropic); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonAzureSignInUnconfigured) {
		t.Errorf("no source: status %d body %q", w.Code, w.Body.String())
	}
}

// ── capture ─────────────────────────────────────────────────────────────────

func TestAzureFoundryCapture_StoresSealedUnderTheCallersNamespace(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()

	w := f.capture(t, subject, azUIDAnthropic)
	if w.Code != http.StatusFound || w.Header().Get("Location") != azureFoundrySignInDonePath {
		t.Fatalf("capture: status %d location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	blob, found := f.stored(t, subject, azUIDAnthropic)
	if !found {
		t.Fatal("nothing was stored under the caller's own principal")
	}
	if !slices.Equal(blob.Scopes, []string{azFoundryScope}) {
		t.Errorf("stored scopes = %v; want exactly the permission under the resource", blob.Scopes)
	}
	if blob.Subject != subject || blob.TenantID != f.app.TenantID || blob.ClientID != f.app.ClientID || blob.RefreshToken == "" {
		t.Errorf("stored blob = %+v", blob)
	}
	raw, _ := json.Marshal(blob)
	if strings.Contains(string(raw), "access_token") {
		t.Errorf("the stored blob carries an access token: %s", raw)
	}
	wantName := providerSecretName(azUIDAnthropic, providerEntraPart)
	if names := f.storedNames(t, subject); !slices.Equal(names, []string{wantName}) {
		t.Errorf("stored names = %v; want exactly %q", names, wantName)
	}
	if names := f.storedNames(t, "someone-else"); len(names) != 0 {
		t.Errorf("another principal holds %v", names)
	}
	// Sealed at every sink.
	if !reservedSecret(wantName) || !sinkReservedSecret(wantName) || !nameSinkReservedSecret(wantName) || !broker.ReservedSecretName(wantName) {
		t.Errorf("%q is not refused at every sink", wantName)
	}

	rows := f.audit.find(azureSignInCapturedAction)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != subject || rows[0].Target != wantName {
		t.Fatalf("audit rows = %+v; want one success row attributed to the caller, targeting the sealed name", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["owner"] != subject || data["provider"] != string(entraKindAzureFoundry) || data["source"] != adoEntraSourceSignIn {
		t.Errorf("audit data = %v", data)
	}
	if strings.Contains(string(rows[0].Data), blob.RefreshToken) {
		t.Error("the audit row carries the refresh token")
	}
	if len(f.audit.find(adoSignInCapturedAction)) != 0 {
		t.Error("an Azure capture wrote the Azure DevOps action")
	}
	registered := false
	for _, v := range f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil) {
		if string(v) == blob.RefreshToken {
			registered = true
		}
	}
	if !registered {
		t.Error("the captured refresh token is not registered with the mask registry")
	}
}

// TestAzureFoundryCapture_TwoRowsOnePersonTwoBlobs: two rows signed in by one
// person produce two distinct sealed blobs, each for its own audience.
func TestAzureFoundryCapture_TwoRowsOnePersonTwoBlobs(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.capture(t, subject, azUIDAnthropic)
	f.capture(t, subject, azUIDOpenAI)

	names := f.storedNames(t, subject)
	slices.Sort(names)
	want := []string{providerSecretName(azUIDAnthropic, providerEntraPart), providerSecretName(azUIDOpenAI, providerEntraPart)}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("stored names = %v; want %v", names, want)
	}
	a, _ := f.stored(t, subject, azUIDAnthropic)
	b, _ := f.stored(t, subject, azUIDOpenAI)
	if a.RefreshToken == b.RefreshToken || !slices.Equal(a.Scopes, []string{azFoundryScope}) || !slices.Equal(b.Scopes, []string{azCognitiveScope}) {
		t.Fatalf("the two rows' blobs are not distinct: %+v / %+v", a, b)
	}
}

// TestAzureFoundryCallback_RowIsTheCookiesNeverTheQuerys: a callback whose query
// carries another row's uid stores under the cookie's row only.
func TestAzureFoundryCallback_RowIsTheCookiesNeverTheQuerys(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
	for _, key := range []string{"uid", "id", "provider", "row"} {
		q.Set(key, azUIDOpenAI)
	}
	if w := f.callback(subject, q, cookies); w.Header().Get("Location") != azureFoundrySignInDonePath {
		t.Fatalf("callback: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	if _, found := f.stored(t, subject, azUIDAnthropic); !found {
		t.Error("nothing was stored under the cookie's row")
	}
	if _, found := f.stored(t, subject, azUIDOpenAI); found {
		t.Error("the grant was stored under the query's row")
	}
}

// TestAzureFoundryCallback_RefusesARowThatIsNoLongerWhatStarted: a cookie naming
// a deleted row, a non-Azure row, or a row whose route changed after start is
// refused, stores nothing, and audits a failure.
func TestAzureFoundryCallback_RefusesARowThatIsNoLongerWhatStarted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*types.ModelProviders)
	}{
		{"deleted", func(b *types.ModelProviders) {
			b.Providers = slices.DeleteFunc(b.Providers, func(p types.ModelProvider) bool { return p.UID == azUIDAnthropic })
		}},
		{"another kind", func(b *types.ModelProviders) {
			for i := range b.Providers {
				if b.Providers[i].UID == azUIDAnthropic {
					b.Providers[i].Kind, b.Providers[i].Azure = types.ModelProviderAnthropicAPIKey, nil
				}
			}
		}},
		{"route changed", func(b *types.ModelProviders) {
			for i := range b.Providers {
				if b.Providers[i].UID == azUIDAnthropic {
					az := *b.Providers[i].Azure
					az.Route = types.AzureRouteOpenAIV1
					b.Providers[i].Azure = &az
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFixture(t)
			subject := f.fake.Subject()
			q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
			f.site.set(tc.mutate)
			w := f.callback(subject, q, cookies)
			if w.Code != http.StatusFound || w.Header().Get("Location") != azureErrorRedirect(azureCaptureRowChanged) {
				t.Fatalf("callback: status %d location %q body %q; want a redirect to %s", w.Code, w.Header().Get("Location"), w.Body.String(), azureCaptureRowChanged)
			}
			if names := f.storedNames(t, subject); len(names) != 0 {
				t.Fatalf("a refused capture stored %v", names)
			}
			rows := f.audit.find(azureSignInCapturedAction)
			if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), azureCaptureRowChanged) {
				t.Fatalf("audit rows = %+v; want one failure row_changed", rows)
			}
		})
	}
}

// TestAzureFoundryCallback_RefusesAForgedStamp: the state cookie is unsigned, so
// a stamp edited in the browser is held to the live row: a different digest or
// an audience the row's route does not name stores nothing.
func TestAzureFoundryCallback_RefusesAForgedStamp(t *testing.T) {
	for name, forge := range map[string]func(*azureFoundryState){
		"digest":   func(st *azureFoundryState) { st.Digest = "0" },
		"audience": func(st *azureFoundryState) { st.Audience = azureCognitiveServicesAudience },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAzureFixture(t)
			subject := f.fake.Subject()
			q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
			stamp, ok := decodeAzureFoundryState(cookieNamed(t, cookies, azureStateCookieName).Value)
			if !ok {
				t.Fatal("start set no decodable stamp")
			}
			forge(&stamp)
			w := f.callback(subject, q, withCookie(cookies, azureStateCookieName, stamp.encode()))
			if w.Header().Get("Location") != azureErrorRedirect(azureCaptureRowChanged) {
				t.Fatalf("callback: status %d location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
			}
			if names := f.storedNames(t, subject); len(names) != 0 {
				t.Fatalf("a forged stamp stored %v", names)
			}
		})
	}
	t.Run("a uid that cannot be a store name", func(t *testing.T) {
		f := newAzureFixture(t)
		subject := f.fake.Subject()
		q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
		stamp, _ := decodeAzureFoundryState(cookieNamed(t, cookies, azureStateCookieName).Value)
		stamp.RowUID = "../x"
		w := f.callback(subject, q, withCookie(cookies, azureStateCookieName, stamp.encode()))
		if w.Code != http.StatusBadRequest || len(f.audit.find(azureSignInCapturedAction)) != 0 {
			t.Fatalf("status %d, audit %v; want a 400 and no row", w.Code, f.audit.find(azureSignInCapturedAction))
		}
	})
}

func TestAzureFoundryCallback_AttackShapedRefusalsEmitNoRow(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	q, cookies, _ := f.begin(t, subject, azUIDAnthropic)

	// A forged state falls through to the Azure DevOps callback, which has no
	// source here and answers its own refusal.
	forged := url.Values{"state": {"forged"}, "code": {q.Get("code")}}
	if w := f.callback(subject, forged, cookies); w.Code == http.StatusFound {
		t.Errorf("a forged state was accepted: %d", w.Code)
	}
	// A missing nonce cookie is refused in band.
	var kept []*http.Cookie
	for _, c := range cookies {
		if !strings.HasSuffix(c.Name, azureNonceCookieName) {
			kept = append(kept, c)
		}
	}
	if w := f.callback(subject, q, kept); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonAzureCallbackCookiesInvalid) {
		t.Errorf("missing nonce: status %d body %q", w.Code, w.Body.String())
	}
	// No session subject.
	if w := f.callback("", q, cookies); w.Code != http.StatusForbidden {
		t.Errorf("no session: status %d", w.Code)
	}
	if rows := f.audit.find(azureSignInCapturedAction); len(rows) != 0 {
		t.Errorf("attack-shaped refusals wrote audit rows: %+v", rows)
	}
	if names := f.storedNames(t, subject); len(names) != 0 {
		t.Errorf("stored %v", names)
	}
}

func TestAzureFoundryCallback_RefusesASubjectMismatch(t *testing.T) {
	f := newAzureFixture(t)
	f.fake.SetSubject("someone-elses-subject")
	w := f.capture(t, "the-session-subject", azUIDAnthropic)
	if w.Header().Get("Location") != azureErrorRedirect(reasonADOCallbackIdentityBinding) {
		t.Fatalf("callback: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	if names := f.storedNames(t, "the-session-subject"); len(names) != 0 {
		t.Fatalf("stored %v", names)
	}
}

// TestAzureFoundryCapture_AnAnswerNamingOnlyAnotherResourceIsUnusable: the
// tenant answers a `.default` request with permissions of a different resource.
func TestAzureFoundryCapture_AnAnswerNamingOnlyAnotherResourceIsUnusable(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.fake.SetAnswerScope("openid", "offline_access", azCognitiveScope)
	w := f.capture(t, subject, azUIDAnthropic)
	if w.Header().Get("Location") != azureErrorRedirect(reasonADOCallbackUnusableGrant) {
		t.Fatalf("callback: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	if names := f.storedNames(t, subject); len(names) != 0 {
		t.Fatalf("an unusable grant stored %v", names)
	}
}

func TestAzureFoundryCapture_AuthorityRefusalsRedirectWithTheirCode(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.fake.SetConsentRequired(true)
	w := f.capture2(t, subject, azUIDAnthropic)
	if w.Header().Get("Location") != azureErrorRedirect(string(ADOEntraFailureConsentRequired)) {
		t.Fatalf("callback: status %d location %q", w.Code, w.Header().Get("Location"))
	}
	if rows := f.audit.find(azureSignInCapturedAction); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

// capture2 is capture for an authority that refuses at /authorize: the redirect
// back carries `error` and no code.
func (f *azureFixture) capture2(t *testing.T, subject, uid string) *httptest.ResponseRecorder {
	t.Helper()
	w := f.start(subject, uid)
	if w.Code != http.StatusFound {
		t.Fatalf("start: status %d", w.Code)
	}
	return f.callback(subject, follow(t, w.Header().Get("Location")), w.Result().Cookies())
}

// ── redemption ──────────────────────────────────────────────────────────────

// TestAzureFoundryRedeem_UsesTheResourceDefaultAndRotates: a `.default` request
// is answered with the resource's permission; it is redeemable, the answer is
// the granted permission set (never the literal), and the rotated refresh token
// is persisted.
func TestAzureFoundryRedeem_UsesTheResourceDefaultAndRotates(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.capture(t, subject, azUIDAnthropic)
	before, _ := f.stored(t, subject, azUIDAnthropic)

	cfg := f.app
	cfg.RowID = azUIDAnthropic
	access, err := f.srv.RedeemAzureFoundryAccess(context.Background(), cfg, subject, azUIDAnthropic, azureFoundryAudience)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if access.AccessToken == "" || !slices.Equal(access.Scopes, []string{azFoundryScope}) {
		t.Fatalf("access = %+v; want a token for exactly %s", access, azFoundryScope)
	}
	carried, ok := f.fake.ScopesForAccessToken(access.AccessToken)
	if !ok || slices.Contains(carried, azCognitiveScope) {
		t.Fatalf("the token carries %v (known=%v)", carried, ok)
	}
	after, _ := f.stored(t, subject, azUIDAnthropic)
	if after.RefreshToken == before.RefreshToken || after.RenewedAt.IsZero() {
		t.Errorf("the rotation was not persisted: before %q after %q renewed %v", before.RefreshToken, after.RefreshToken, after.RenewedAt)
	}
	if live, known := f.fake.RefreshTokenState(before.RefreshToken); live || !known {
		t.Errorf("the spent refresh token is live=%v known=%v", live, known)
	}
	// Redeemable again from the rotated token.
	if _, err := f.srv.RedeemAzureFoundryAccess(context.Background(), cfg, subject, azUIDAnthropic, azureFoundryAudience); err != nil {
		t.Fatalf("second redeem: %v", err)
	}
}

func TestAzureFoundryRedeem_RefusesBeforeBuildingARequest(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.capture(t, subject, azUIDAnthropic)
	var issued int
	f.fake.OnIssue(func(entrafake.IssuedToken) { issued++ })
	cfg := f.app
	cfg.RowID = azUIDAnthropic
	ctx := context.Background()

	for name, aud := range map[string]string{
		"an audience outside the closed set": "https://example.invalid/.default",
		"a bare resource":                    "https://ai.azure.com",
		"empty":                              "",
	} {
		if _, err := f.srv.RedeemAzureFoundryAccess(ctx, cfg, subject, azUIDAnthropic, aud); err == nil {
			t.Errorf("%s was redeemed", name)
		}
	}
	// A closed audience the capture holds no scope under is a consent refusal,
	// answered locally.
	_, err := f.srv.RedeemAzureFoundryAccess(ctx, cfg, subject, azUIDAnthropic, azureCognitiveServicesAudience)
	if !errors.Is(err, ErrADOEntraConsentRequired) {
		t.Errorf("the other audience: err = %v; want consent required", err)
	}
	if _, err := f.srv.RedeemAzureFoundryAccess(ctx, cfg, "", azUIDAnthropic, azureFoundryAudience); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Errorf("no owner: err = %v", err)
	}
	if _, err := f.srv.RedeemAzureFoundryAccess(ctx, cfg, "nobody", azUIDAnthropic, azureFoundryAudience); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Errorf("no capture: err = %v", err)
	}
	other := cfg
	other.RowID = azUIDOpenAI
	if _, err := f.srv.RedeemAzureFoundryAccess(ctx, other, subject, azUIDAnthropic, azureFoundryAudience); err == nil {
		t.Error("a configuration for another row was accepted")
	}
	if issued != 0 {
		t.Errorf("the authority minted %d token(s) for refused redemptions", issued)
	}
}

// TestAzureFoundryRedeem_ADeadGrantIsDeletedSealed: an invalid_grant answer
// retires the stored -entra blob, as any provider sign-in does.
func TestAzureFoundryRedeem_ADeadGrantIsDeletedSealed(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()
	f.capture(t, subject, azUIDAnthropic)
	f.fake.SetInvalidGrant(true)
	cfg := f.app
	cfg.RowID = azUIDAnthropic
	_, err := f.srv.RedeemAzureFoundryAccess(context.Background(), cfg, subject, azUIDAnthropic, azureFoundryAudience)
	if ADOEntraClassify(err) != ADOEntraFailureDeadCredential {
		t.Fatalf("err = %v; want a dead credential", err)
	}
	if _, found := f.stored(t, subject, azUIDAnthropic); found {
		t.Error("the dead sign-in is still stored")
	}
}

// ── names, purge, inventory ─────────────────────────────────────────────────

func TestAzureFoundryEntraNameIsEnumeratedAndInventoried(t *testing.T) {
	if !slices.Contains(providerSecretParts, providerEntraPart) {
		t.Fatal("providerSecretParts does not enumerate the -entra part")
	}
	p := azureRows()[0]
	want := providerSecretName(p.UID, providerEntraPart)
	if got := providerCredentialName(p); got != want {
		t.Errorf("providerCredentialName = %q; want %q", got, want)
	}
	if !strings.HasSuffix(want, "-entra") || !providerSignInSecret(want) {
		t.Errorf("%q is not a sealed provider sign-in", want)
	}
	// A typed key is still not sealed at the sinks.
	if providerSignInSecret(providerSecretName(p.UID, providerKeyPart)) {
		t.Error("a -key name is sealed as a sign-in")
	}
}
