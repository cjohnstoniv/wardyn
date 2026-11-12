// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	oidcUID   = "22222222-bbbb-4bbb-8bbb-000000000001"
	oidcOther = "22222222-bbbb-4bbb-8bbb-000000000002"
)

// oidcRow is a custom_endpoint that signs people in, as an admin writes it.
func oidcRow() types.ModelProvider {
	p := endpointProvider()
	p.UID = oidcUID
	p.OIDC = &types.ProviderOIDC{
		Issuer: "https://idp.corp.example", ClientID: "wardyn-client", Scopes: []string{"api.read", "api.write"},
	}
	return p
}

// ── the row ─────────────────────────────────────────────────────────────────

func TestEndpointOIDC_ValidationTable(t *testing.T) {
	with := func(edit func(*types.ModelProvider)) types.ModelProvider {
		p := oidcRow()
		edit(&p)
		return p
	}
	many := make([]string, adoEntraMaxScopes+1)
	for i := range many {
		many[i] = fmt.Sprintf("scope%d", i)
	}
	for _, tc := range []struct {
		name string
		row  types.ModelProvider
		want string // "" = accepted; else a substring of the refusal
	}{
		{"a complete block", oidcRow(), ""},
		{"binding none", with(func(p *types.ModelProvider) { p.OIDC.IdentityBinding = types.OIDCBindingNone }), ""},
		{"binding console", with(func(p *types.ModelProvider) { p.OIDC.IdentityBinding = types.OIDCBindingConsole }), ""},
		{"endpoint overrides", with(func(p *types.ModelProvider) {
			p.OIDC.AuthorizeURL, p.OIDC.TokenURL = "https://idp.corp.example/authorize", "https://idp.corp.example/token"
		}), ""},
		{"a client secret ref", with(func(p *types.ModelProvider) { p.OIDC.ClientSecretRef = "idp-client-secret" }), ""},
		{"only custom_endpoint", with(func(p *types.ModelProvider) {
			p.Kind = types.ModelProviderAnthropicAPIKey
			p.BaseURL = ""
			p.Harnesses = nil
		}), "custom_endpoint"},
		{"issuer is required", with(func(p *types.ModelProvider) { p.OIDC.Issuer = "" }), "oidc.issuer is required"},
		{"issuer must be https", with(func(p *types.ModelProvider) { p.OIDC.Issuer = "http://idp.corp.example" }), "must be https"},
		{"issuer carries no credential", with(func(p *types.ModelProvider) { p.OIDC.Issuer = "https://u:p@idp.corp.example" }), "credential"},
		{"issuer carries no fragment", with(func(p *types.ModelProvider) { p.OIDC.Issuer = "https://idp.corp.example/#x" }), "fragment"},
		{"issuer is not a metadata address", with(func(p *types.ModelProvider) { p.OIDC.Issuer = "https://169.254.169.254" }), "link-local"},
		{"authorize override must be https", with(func(p *types.ModelProvider) { p.OIDC.AuthorizeURL = "http://idp.corp.example/a" }), "oidc.authorize_url"},
		{"token override carries no query", with(func(p *types.ModelProvider) { p.OIDC.TokenURL = "https://idp.corp.example/t?x=1" }), "query"},
		{"client id is required", with(func(p *types.ModelProvider) { p.OIDC.ClientID = "" }), "client_id"},
		{"client id has no space", with(func(p *types.ModelProvider) { p.OIDC.ClientID = "a b" }), "client_id"},
		{"client secret ref is a plain name", with(func(p *types.ModelProvider) { p.OIDC.ClientSecretRef = "../etc" }), "client_secret_ref"},
		{"client secret ref is not a sealed name", with(func(p *types.ModelProvider) { p.OIDC.ClientSecretRef = "wardyn-provider-x-key" }), "client_secret_ref"},
		{"scopes are required", with(func(p *types.ModelProvider) { p.OIDC.Scopes = nil }), "oidc.scopes"},
		{"scopes are bounded", with(func(p *types.ModelProvider) { p.OIDC.Scopes = many }), "oidc.scopes"},
		{"a scope has no whitespace", with(func(p *types.ModelProvider) { p.OIDC.Scopes = []string{"a b"} }), "oidc.scopes"},
		{"no .default", with(func(p *types.ModelProvider) { p.OIDC.Scopes = []string{"https://x.example/.default"} }), "oidc.scopes"},
		{"binding is one of two", with(func(p *types.ModelProvider) { p.OIDC.IdentityBinding = "email" }), "identity_binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateModelProviders(providerBlock(tc.row), providerWriteEnv{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	t.Run("the test posture admits a plain-http authority", func(t *testing.T) {
		p := with(func(p *types.ModelProvider) { p.OIDC.Issuer = "http://idp.corp.example" })
		if _, err := validateModelProviders(providerBlock(p), providerWriteEnv{AllowTestEndpoints: true}); err != nil {
			t.Fatalf("refused under the test posture: %v", err)
		}
	})
}

func TestEndpointOIDC_NormalizeTrims(t *testing.T) {
	p := oidcRow()
	p.OIDC.Issuer, p.OIDC.ClientID, p.OIDC.Scopes = "  https://idp.corp.example ", " c ", []string{" api.read "}
	got := normalizeModelProviders(providerBlock(p)).Providers[0].OIDC
	if got.Issuer != "https://idp.corp.example" || got.ClientID != "c" || got.Scopes[0] != "api.read" {
		t.Fatalf("normalized = %+v", got)
	}
}

func TestEndpointOIDC_BindingAndFlag(t *testing.T) {
	var none *types.ProviderOIDC
	if none.Unbound() || none.Binding() != types.OIDCBindingConsole {
		t.Fatal("no block is not an unbound sign-in")
	}
	for _, tc := range []struct {
		set       string
		binding   string
		unbound   bool
		auditsSet bool
	}{
		{"", types.OIDCBindingConsole, false, false},
		{types.OIDCBindingConsole, types.OIDCBindingConsole, false, false},
		{types.OIDCBindingNone, types.OIDCBindingNone, true, true},
	} {
		o := &types.ProviderOIDC{IdentityBinding: tc.set}
		if o.Binding() != tc.binding || o.Unbound() != tc.unbound {
			t.Errorf("binding %q = (%q, %v), want (%q, %v)", tc.set, o.Binding(), o.Unbound(), tc.binding, tc.unbound)
		}
		row := oidcRow()
		row.OIDC.IdentityBinding = tc.set
		data := modelProviderAuditData(types.ModelProviders{}, types.ModelProviders{Providers: []types.ModelProvider{row}})
		_, named := data["oidc_unbound"]
		if named != tc.auditsSet {
			t.Errorf("binding %q: model_provider.write names oidc_unbound = %v, want %v", tc.set, named, tc.auditsSet)
		}
	}
}

// ── the digest and rule 8 ───────────────────────────────────────────────────

func TestEndpointOIDC_DigestAndPurgePredicate(t *testing.T) {
	// No other row's digest moves: a row with no block hashes exactly the inputs it always has.
	plain := endpointProvider()
	harnesses := map[string][2]string{}
	for _, h := range plain.Harnesses {
		harnesses[h.Harness] = [2]string{h.Path, h.AuthHeader}
	}
	sum := sha256.Sum256(mustJSON([]any{plain.BaseURL, bedrockAddress(plain), authHeader(plain.Auth), harnesses}))
	if got := providerAddressDigest(plain); got != hex.EncodeToString(sum[:]) {
		t.Errorf("a row with no oidc block hashes differently than before: %s", got)
	}

	base := oidcRow()
	for name, edit := range map[string]func(*types.ModelProvider){
		"adds the block":    func(p *types.ModelProvider) { p.OIDC = nil },
		"issuer":            func(p *types.ModelProvider) { p.OIDC.Issuer = "https://other.corp.example" },
		"authorize url":     func(p *types.ModelProvider) { p.OIDC.AuthorizeURL = "https://idp.corp.example/a" },
		"token url":         func(p *types.ModelProvider) { p.OIDC.TokenURL = "https://idp.corp.example/t" },
		"client id":         func(p *types.ModelProvider) { p.OIDC.ClientID = "another" },
		"a scope":           func(p *types.ModelProvider) { p.OIDC.Scopes = []string{"api.read"} },
		"identity binding":  func(p *types.ModelProvider) { p.OIDC.IdentityBinding = types.OIDCBindingNone },
		"the base url":      func(p *types.ModelProvider) { p.BaseURL = "https://elsewhere.corp.example" },
		"the header scheme": func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{Header: "X-Token", Format: "%s"} },
	} {
		next := oidcRow()
		edit(&next)
		if providerAddressDigest(base) == providerAddressDigest(next) {
			t.Errorf("%s: the digest did not move", name)
		}
		if !providerAddressChanged(base, next) {
			t.Errorf("%s: rule 8 does not purge", name)
		}
		if !slices.Contains(invalidatedProviderUIDs(providerBlock(base), providerBlock(next)), oidcUID) {
			t.Errorf("%s: the row's credentials are not invalidated", name)
		}
	}

	same := oidcRow()
	same.OIDC.Scopes = []string{"api.write", "api.read"} // a reordered set is the same set
	same.OIDC.ClientSecretRef = "rotated-secret"         // a secret rotation is not a new authority
	same.Name = "Renamed"
	if providerAddressDigest(base) != providerAddressDigest(same) || providerAddressChanged(base, same) {
		t.Error("a reordered scope set, a new secret ref or a rename purged people's sign-ins")
	}
}

// ── the sealed part ─────────────────────────────────────────────────────────

func TestEndpointOIDC_SealedPart(t *testing.T) {
	name := providerSecretName(oidcUID, providerOIDCPart)
	if name != "wardyn-provider-"+oidcUID+"-oidc" {
		t.Fatalf("sealed name = %q", name)
	}
	if !slices.Contains(providerSecretParts, providerOIDCPart) {
		t.Error("the part is not purged, counted and inventoried with the others")
	}
	for label, reserved := range map[string]bool{
		"providerSignInSecret": providerSignInSecret(name), "reservedSecret": reservedSecret(name),
		"sinkReservedSecret": sinkReservedSecret(name), "secretsAPIReserved": secretsAPIReserved(name),
		"nameSinkReservedSecret": nameSinkReservedSecret(name),
	} {
		if !reserved {
			t.Errorf("%s does not seal %s", label, name)
		}
	}
	if got := providerCredentialName(oidcRow()); got != name {
		t.Errorf("an oidc row's credential is %q, want the sealed part", got)
	}
	plain := endpointProvider()
	plain.UID = oidcUID
	if got := providerCredentialName(plain); got != providerSecretName(oidcUID, providerKeyPart) {
		t.Errorf("a row with no oidc block names %q, want its -key", got)
	}
}

// The paste door refuses a row that signs people in, and a person's disconnect still reaches the sign-in.
func TestEndpointOIDC_PasteDoorRefusesAndDisconnectDeletes(t *testing.T) {
	site := credentialSite(oidcRowForSite())
	uid := site.ModelProviders.Providers[0].UID
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	mem := srv.cfg.Secrets.(*memSecrets)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/corp-gateway/credential", member, `{"value":"a-pasted-token-0123456789"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonModelProviderCredentialSignsIn) {
		t.Fatalf("PUT = %d %s, want 409 %s", w.Code, w.Body.String(), reasonModelProviderCredentialSignsIn)
	}
	if providerRows(mem, uid) != 0 {
		t.Fatal("a refused paste stored a row")
	}

	if err := mem.For("sub-member").Put(t.Context(), providerSecretName(uid, providerOIDCPart), []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	w = doSSO(t, srv, http.MethodDelete, "/api/v1/model-providers/corp-gateway/credential", member, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if providerRows(mem, uid) != 0 {
		t.Fatal("the person's disconnect left their sign-in behind")
	}
}

func oidcRowForSite() types.ModelProvider {
	p := oidcRow()
	p.UID = "" // credentialSite mints it
	return p
}

// ── the token-endpoint seam ─────────────────────────────────────────────────

// An Entra application's token request is byte for byte what it was before the seam; the seam's other
// client authentication keeps the credentials out of the form.
func TestEndpointOIDC_PostTokenSeam(t *testing.T) {
	var gotBody, gotAuth, gotType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotType = string(b), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a-0123456789","expires_in":3600}`)
	}))
	defer ts.Close()
	s := &Server{cfg: Config{}}
	ctx := context.Background()
	form := func() url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"r-1"}, "scope": {"a b"}}
	}
	cfg := ADOEntraConfig{TenantID: "tenant-1", ClientID: "client-1", ClientSecret: "s3cret", AuthorityOverride: ts.URL, AllowTestEndpoints: true}

	if _, err := s.postADOEntraToken(ctx, cfg, form()); err != nil {
		t.Fatal(err)
	}
	if want := "client_id=client-1&client_secret=s3cret&grant_type=refresh_token&refresh_token=r-1&scope=a+b"; gotBody != want || gotAuth != "" {
		t.Errorf("Entra request = %q auth %q, want %q and no Authorization header", gotBody, gotAuth, want)
	}
	if gotType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", gotType)
	}
	cfg.ClientSecret = ""
	if _, err := s.postADOEntraToken(ctx, cfg, form()); err != nil {
		t.Fatal(err)
	}
	if want := "client_id=client-1&grant_type=refresh_token&refresh_token=r-1&scope=a+b"; gotBody != want {
		t.Errorf("a public client sent %q, want %q", gotBody, want)
	}

	ta := tokenAuthority{tokenURL: ts.URL, clientID: "id 1", clientSecret: "se:cret", basicAuth: true}
	if _, err := s.postToken(ctx, ta, form()); err != nil {
		t.Fatal(err)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("id+1:se%3Acret")); gotAuth != want {
		t.Errorf("Authorization = %q, want the form-encoded id and secret joined: %q", gotAuth, want)
	}
	if want := "grant_type=refresh_token&refresh_token=r-1&scope=a+b"; gotBody != want {
		t.Errorf("a Basic request sent %q, want no client credentials in the form: %q", gotBody, want)
	}
}

// Condition: a token response with no scope is read as the requested set FOR THE ENDPOINT KIND ONLY. Every
// Entra capture still refuses it, at the code exchange and at a redemption.
func TestEndpointOIDC_EntraStillRefusesAResponseWithNoScope(t *testing.T) {
	ado := adoCapture(ADOEntraConfig{RowID: "ado-row-1", Scopes: []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code"}})
	foundry, err := azureFoundryCapture(azUIDAnthropic, azureFoundryAudience)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := entraCapture{kind: entraKindEndpointOIDC, scopes: []string{"openid", "offline_access", "api.read"}}
	for name, ec := range map[string]entraCapture{"azure devops": ado, "azure foundry": foundry} {
		if got := ec.capturedScopes(""); len(got) != 0 {
			t.Errorf("%s keeps %v from a response with no scope", name, got)
		}
		if ec.omittedScopeIsGranted() || !ec.sendsScopeOnRefresh() {
			t.Errorf("%s reads an omitted scope as granted, or refreshes without one", name)
		}
	}
	if got := endpoint.capturedScopes(""); !slices.Equal(got, endpoint.scopes) {
		t.Errorf("an endpoint capture keeps %v from a response with no scope, want the requested set %v", got, endpoint.scopes)
	}
	if got := endpoint.capturedScopes("api.read api.read openid"); !slices.Equal(got, []string{"api.read", "openid"}) {
		t.Errorf("a named grant is kept as granted, deduplicated: %v", got)
	}
	if !endpoint.omittedScopeIsGranted() || endpoint.sendsScopeOnRefresh() {
		t.Error("an endpoint capture refreshes with a scope, or refuses an omitted one")
	}

	t.Run("azure devops code exchange", func(t *testing.T) {
		f := newADOFixture(t)
		f.fake.SetGenericMode(true) // the fake now omits scope, which Entra never does
		w := f.capture(t, f.fake.Subject())
		if want := f.srv.cfg.BasePath + adoSignInErrorPath + reasonADOCallbackUnusableGrant; w.Header().Get("Location") != want {
			t.Fatalf("callback Location %q, want %q", w.Header().Get("Location"), want)
		}
		if raw := f.storedRaw(t, f.fake.Subject()); raw != nil {
			t.Error("a sign-in with no granted scope was stored")
		}
	})
	t.Run("azure foundry code exchange", func(t *testing.T) {
		f := newAzureFixture(t)
		f.fake.SetGenericMode(true)
		w := f.capture(t, f.fake.Subject(), azUIDAnthropic)
		if want := f.srv.cfg.BasePath + azureErrorRedirect(reasonADOCallbackUnusableGrant); w.Header().Get("Location") != want {
			t.Fatalf("callback Location %q, want %q", w.Header().Get("Location"), want)
		}
		if _, found := f.stored(t, f.fake.Subject(), azUIDAnthropic); found {
			t.Error("a sign-in with no granted scope was stored")
		}
	})
	t.Run("azure devops redemption", func(t *testing.T) {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		f.capture(t, subject)
		f.fake.SetGenericMode(true)
		_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes[:1])
		if !errors.Is(err, ErrADOEntraUnavailable) || !strings.Contains(err.Error(), "named no granted scope") {
			t.Fatalf("redeem = %v, want it unavailable for naming no granted scope", err)
		}
	})
}
