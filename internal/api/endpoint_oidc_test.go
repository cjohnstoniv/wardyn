// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
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
