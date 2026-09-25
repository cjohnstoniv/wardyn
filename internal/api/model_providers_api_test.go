// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestModelProvidersGet: a never-configured install reads {} with an ETag.
func TestModelProvidersGet(t *testing.T) {
	srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	w := do(t, srv, http.MethodGet, "/api/v1/model-providers", adminToken, "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" || w.Header().Get("ETag") == "" {
		t.Fatalf("GET = %d %q etag=%q, want 200 {} with an ETag", w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
}

// TestModelProvidersGetConnectedPeople (#970): each provider's count of the
// distinct people holding a credential of their own for it — a key or a
// sign-in, one person once; never the operator namespace, never another
// provider's rows — read without moving the ETag, and never by a member.
func TestModelProvidersGetConnectedPeople(t *testing.T) {
	site := credentialSite(keyProvider("anthropic", "claude-code"), openAIProvider(), ssoProvider())
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	mem := srv.cfg.Secrets.(*memSecrets)
	ctx := context.Background()
	name := func(i int, part string) string { return providerSecretName(site.ModelProviders.Providers[i].UID, part) }
	put := func(owner string, i int, part string) {
		t.Helper()
		if err := mem.For(owner).Put(ctx, name(i, part), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	read := func(want map[string]int) string {
		t.Helper()
		w := do(t, srv, http.MethodGet, "/api/v1/model-providers", adminToken, "")
		var body modelProvidersRead
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Providers) != 3 {
			t.Fatalf("GET = %d; body=%s", w.Code, w.Body.String())
		}
		if !maps.Equal(body.ConnectedPeople, want) {
			t.Errorf("connected_people = %v, want %v", body.ConnectedPeople, want)
		}
		return w.Header().Get("ETag")
	}

	etag := read(map[string]int{"anthropic": 0, "openai": 0, "bedrock-prod": 0})
	put("alice", 0, providerKeyPart)
	if read(map[string]int{"anthropic": 1, "openai": 0, "bedrock-prod": 0}) != etag {
		t.Error("a person connecting moved the ETag, which would stale an admin's edit")
	}
	put("alice", 0, providerOAuthPart)
	put("bob", 0, providerKeyPart)
	put("carol", 0, providerSSOPart)
	put("dave", 1, providerKeyPart)
	put("", 0, providerKeyPart) // the operator namespace is nobody
	read(map[string]int{"anthropic": 3, "openai": 1, "bedrock-prod": 0})
	if err := mem.For("bob").Delete(ctx, name(0, providerKeyPart)); err != nil {
		t.Fatal(err)
	}
	read(map[string]int{"anthropic": 2, "openai": 1, "bedrock-prod": 0})

	t.Run("a member never reads it", func(t *testing.T) {
		member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
		if w := doSSO(t, srv, http.MethodGet, "/api/v1/model-providers", member, ""); w.Code != http.StatusForbidden {
			t.Errorf("member GET = %d, want 403", w.Code)
		}
	})

	t.Run("it is never written back", func(t *testing.T) {
		body := `{"providers":[],"connected_people":{"anthropic":9}}`
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body); w.Code != http.StatusBadRequest {
			t.Errorf("PUT with connected_people = %d, want 400", w.Code)
		}
	})

	t.Run("a store that cannot answer fails the read", func(t *testing.T) {
		srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{cfg: site})
		srv.cfg.Secrets = wedgedSecrets{err: errors.New("store down")}
		if w := do(t, srv, http.MethodGet, "/api/v1/model-providers", adminToken, ""); w.Code != http.StatusInternalServerError {
			t.Errorf("GET = %d, want 500", w.Code)
		}
	})
}

// TestModelProvidersPut is the write: it persists one sub-object, mints UIDs,
// audits without addresses, honours If-Match, and {} clears.
func TestModelProvidersPut(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"github.com"}}}
	srv, audit := newSiteConfigHarness(t, fake)

	const startURL = "https://acme.awsapps.com/start"
	body := `{"providers":[` +
		`{"id":"corp-gateway","kind":"custom_endpoint","base_url":"https://gateway.corp.example/llm",` +
		`"harnesses":[{"harness":"claude-code","path":"/anthropic"},{"harness":"codex-cli","path":"/v1"}]},` +
		`{"id":"bedrock-prod","kind":"bedrock_sso","disabled":true,"bedrock":{"region":"us-east-1","sso_start_url":"` + startURL + `",` +
		`"sso_account_id":"123456789012","sso_role_name":"BedrockUser"},"harnesses":[{"harness":"claude-code","model":"us.anthropic.claude-sonnet-4-5-20250929-v1:0"}]}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	got := fake.putSeen.ModelProviders.Providers
	if len(got) != 2 || got[0].UID == "" || got[1].UID == "" || len(fake.putSeen.ScmHosts) != 1 {
		t.Fatalf("stored %+v (scm_hosts %v), want both providers with UIDs and the rest untouched", got, fake.putSeen.ScmHosts)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("PUT echoes no ETag")
	}
	datum := modelProviderWriteDatum(t, audit)
	raw, _ := json.Marshal(datum)
	for _, leak := range []string{startURL, "gateway.corp.example"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("model_provider.write carries %q: %s", leak, raw)
		}
	}
	want := `{"base_url_changed":[],"disabled":["bedrock-prod"],"harnesses":["claude-code:bedrock-prod","claude-code:corp-gateway","codex-cli:corp-gateway"],` +
		`"ids":["bedrock-prod","corp-gateway"],"kind_changed":[],"kinds":["bedrock_sso","custom_endpoint"],"per_user_credentials_invalidated":0,"pins":["bedrock-prod:123456789012/BedrockUser"],"provider_count":2}`
	if string(raw) != want {
		t.Errorf("datum = %s\nwant    %s", raw, want)
	}

	t.Run("an address change is named, and the UID survives it", func(t *testing.T) {
		uid := fake.cfg.ModelProviders.Providers[0].UID
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken,
			`{"providers":[{"id":"corp-gateway","kind":"custom_endpoint","base_url":"https://gateway.corp.example/llm",`+
				`"harnesses":[{"harness":"claude-code","path":"/anthropic/v2"}]}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if fake.cfg.ModelProviders.Providers[0].UID != uid {
			t.Error("an edit reissued the provider's UID")
		}
		if d := modelProviderWriteDatum(t, audit); !reflect.DeepEqual(d["base_url_changed"], []any{"corp-gateway"}) {
			t.Errorf("base_url_changed = %v, want [corp-gateway]", d["base_url_changed"])
		}
	})

	// base_url_changed names exactly the providers whose address or header
	// scheme moved: nothing on an identical re-PUT, and the one provider
	// whose Bedrock region or auth header changed. Each case fails if
	// providerAddressChanged answered the same thing unconditionally.
	t.Run("base_url_changed names only a moved address", func(t *testing.T) {
		seeded := func() *fakeSiteConfigStore {
			block := normalizeModelProviders(providerBlock(endpointProvider(), ssoProvider()))
			assignModelProviderUIDs(block, nil)
			return &fakeSiteConfigStore{cfg: types.SiteConfig{ModelProviders: block}}
		}
		for _, tc := range []struct {
			name string
			edit func(*types.ModelProviders)
			want []any
		}{
			{"an identical re-PUT", func(*types.ModelProviders) {}, []any{}},
			{"the Bedrock region", func(b *types.ModelProviders) { b.Providers[1].Bedrock.Region = "us-west-2" }, []any{"bedrock-prod"}},
			{"the auth header", func(b *types.ModelProviders) { b.Providers[0].Auth.Header = "X-Api-Key" }, []any{"corp-gateway"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fake := seeded()
				srv, audit := newSiteConfigHarness(t, fake)
				body := normalizeModelProviders(providerBlock(endpointProvider(), ssoProvider()))
				tc.edit(body)
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(raw)); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
				}
				if got := modelProviderWriteDatum(t, audit)["base_url_changed"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("base_url_changed = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("a kind change mints a fresh UID and is named", func(t *testing.T) {
		uid := fake.cfg.ModelProviders.Providers[0].UID
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken,
			`{"providers":[{"id":"corp-gateway","kind":"anthropic_api_key","harnesses":[{"harness":"claude-code"}]}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if got := fake.cfg.ModelProviders.Providers[0].UID; got == uid || got == "" {
			t.Errorf("uid = %q after a kind change, want a fresh one (was %q) — stored credentials would follow the id", got, uid)
		}
		if d := modelProviderWriteDatum(t, audit); !reflect.DeepEqual(d["kind_changed"], []any{"corp-gateway"}) {
			t.Errorf("kind_changed = %v, want [corp-gateway]", d["kind_changed"])
		}
	})

	t.Run("a stale If-Match is refused before the write", func(t *testing.T) {
		w := doWithHeaders(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, `{}`, map[string]string{"If-Match": `"stale"`})
		if w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), mp412Stale) {
			t.Fatalf("stale If-Match = %d %s, want 412", w.Code, w.Body.String())
		}
	})

	t.Run("the GET's own ETag satisfies the PUT", func(t *testing.T) {
		etag := do(t, srv, http.MethodGet, "/api/v1/model-providers", adminToken, "").Header().Get("ETag")
		w := doWithHeaders(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken,
			`{"providers":[{"id":"anthropic","kind":"anthropic_api_key"}]}`, map[string]string{"If-Match": etag})
		if w.Code != http.StatusOK {
			t.Fatalf("fresh If-Match = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("an invalid block and an unknown field are refused", func(t *testing.T) {
		for _, b := range []string{`{"providers":[{"id":"a","kind":"bedrock_env"}]}`, `{"bogus":1}`} {
			if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, b); w.Code != http.StatusBadRequest {
				t.Errorf("PUT %s = %d, want 400", b, w.Code)
			}
		}
	})

	t.Run("{} clears", func(t *testing.T) {
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, `{}`); w.Code != http.StatusOK {
			t.Fatalf("clear = %d; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.ModelProviders != nil {
			t.Errorf("model_providers = %+v, want nil", fake.putSeen.ModelProviders)
		}
	})
}

// TestModelProvidersPutKeepsDefaults: removing a default's provider, or
// unticking the agent it defaults, is refused (E8); turning it off is not.
func TestModelProvidersPutKeepsDefaults(t *testing.T) {
	stored := types.SiteConfig{
		ModelProviders: providerBlock(keyProvider("anthropic", "claude-code")),
		AgentProviders: agentBlock(defaultRow("claude-code", "anthropic")),
	}
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"removing it", `{}`, http.StatusBadRequest},
		{"unticking its agent", `{"providers":[{"id":"anthropic","kind":"anthropic_api_key"}]}`, http.StatusBadRequest},
		{"turning it off", `{"providers":[{"id":"anthropic","kind":"anthropic_api_key","disabled":true,"harnesses":[{"harness":"claude-code"}]}]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{cfg: stored})
			w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, tc.body)
			if w.Code != tc.code {
				t.Fatalf("PUT = %d, want %d; body=%s", w.Code, tc.code, w.Body.String())
			}
			if tc.code == http.StatusBadRequest && !strings.Contains(w.Body.String(), "is the default for") {
				t.Errorf("400 body = %s, want the E8 sentence", w.Body.String())
			}
		})
	}
}

const signInRef = "wardyn/agent-claude-code:local"

// signInImageSrv is a server whose store holds stored, with the claude-code
// image pinned to signInRef and rnr wired as its Runner; the fake store is
// returned so a test can read what was saved.
func signInImageSrv(t *testing.T, stored types.SiteConfig, rnr runner.Runner) (*Server, *fakeSiteConfigStore) {
	t.Helper()
	fake := &fakeSiteConfigStore{cfg: stored}
	cfg := baseTestConfig(newHarness(t), fake)
	cfg.AgentImages = map[string]string{"claude-code": signInRef}
	cfg.Runner = rnr
	return New(cfg), fake
}

// signInImageMissing is the Docker runner after the image went missing (a
// prune, a daemon swap, an MDM file applied before the first build).
func signInImageMissing() runner.Runner {
	return &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{}}
}

func subscriptionProvider(id string) types.ModelProvider {
	return types.ModelProvider{ID: id, Kind: types.ModelProviderAnthropicSubscription,
		Harnesses: []types.ProviderHarness{{Harness: "claude-code"}}}
}

// TestModelProvidersPutSubscriptionNeedsSignInImage is the E4 refusal (multi-
// provider design 2.2, 5.2 E4): a PUT that adds an anthropic_subscription
// provider is refused until the Claude sign-in image resolves — proven live
// against the write door, not just the pure validator — while one that is off,
// or already stored, never is: the off switch and every other edit keep working
// after the image goes missing.
func TestModelProvidersPutSubscriptionNeedsSignInImage(t *testing.T) {
	body := `{"providers":[{"id":"claude-sub","kind":"anthropic_subscription","harnesses":[{"harness":"claude-code"}]}]}`

	t.Run("no pin at all: refused", func(t *testing.T) {
		h := newHarness(t)
		cfg := baseTestConfig(h, &fakeSiteConfigStore{})
		srv := New(cfg)
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "sign-in image") {
			t.Errorf("400 body = %s, want the E4 sentence", w.Body.String())
		}
	})

	t.Run("pinned but the Docker runner does not have it yet: refused", func(t *testing.T) {
		srv, _ := signInImageSrv(t, types.SiteConfig{}, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("a pin the daemon does not actually have must still be refused: PUT = %d; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("pinned and present: accepted", func(t *testing.T) {
		srv, _ := signInImageSrv(t, types.SiteConfig{},
			&imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{signInRef: true}})
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("pinned and the runner cannot confirm (k8s): trusted, accepted", func(t *testing.T) {
		srv, _ := signInImageSrv(t, types.SiteConfig{}, imageCheckErrRunner{&fakeRunner{}})
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a subscription that is off is never refused", func(t *testing.T) {
		srv, fake := signInImageSrv(t, types.SiteConfig{}, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken,
			`{"providers":[{"id":"claude-sub","kind":"anthropic_subscription","disabled":true,"harnesses":[{"harness":"claude-code"}]}]}`)
		if w.Code != http.StatusOK || !fake.putSeen.ModelProviders.Providers[0].Disabled {
			t.Fatalf("PUT = %d, want 200 with the provider stored off; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a stored subscription never blocks an edit to another provider", func(t *testing.T) {
		stored := types.SiteConfig{ModelProviders: providerBlock(subscriptionProvider("claude-sub"), keyProvider("anthropic", "claude-code"))}
		srv, fake := signInImageSrv(t, stored, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken,
			`{"providers":[{"id":"claude-sub","kind":"anthropic_subscription","harnesses":[{"harness":"claude-code"}]},`+
				`{"id":"anthropic","name":"Anthropic key","kind":"anthropic_api_key","harnesses":[{"harness":"claude-code"}]}]}`)
		if w.Code != http.StatusOK || fake.putSeen.ModelProviders.Providers[1].Name != "Anthropic key" {
			t.Fatalf("PUT = %d, want 200 with the edit saved; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("turning a stored-off subscription back on adds one: refused", func(t *testing.T) {
		off := subscriptionProvider("claude-sub")
		off.Disabled = true
		srv, fake := signInImageSrv(t, types.SiteConfig{ModelProviders: providerBlock(off)}, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sign-in image") {
			t.Fatalf("PUT = %d, want 400 with the E4 sentence; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen != nil {
			t.Fatal("a refused PUT must not write")
		}
	})

	t.Run("a stored id changing kind to a subscription adds one: refused", func(t *testing.T) {
		stored := types.SiteConfig{ModelProviders: providerBlock(keyProvider("claude-sub", "claude-code"))}
		srv, _ := signInImageSrv(t, stored, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sign-in image") {
			t.Fatalf("PUT = %d, want 400 with the E4 sentence; body=%s", w.Code, w.Body.String())
		}
	})
}

// TestSiteConfigPutSubscriptionNeedsSignInImage is E4 at the site-config door:
// refused when the document adds a subscription, never when it only carries a
// stored one — console saves spread the GET document, and deploy/desktop
// re-applies the MDM file on every boot.
func TestSiteConfigPutSubscriptionNeedsSignInImage(t *testing.T) {
	body := `{"scm_hosts":["github.com"],"model_providers":{"providers":[` +
		`{"id":"claude-sub","kind":"anthropic_subscription","harnesses":[{"harness":"claude-code"}]}]}}`

	t.Run("a new subscription, pinned but absent: refused", func(t *testing.T) {
		srv, _ := signInImageSrv(t, types.SiteConfig{}, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sign-in image") {
			t.Fatalf("PUT = %d, want 400 with the E4 sentence; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a new subscription, pinned and present: accepted", func(t *testing.T) {
		srv, _ := signInImageSrv(t, types.SiteConfig{},
			&imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{signInRef: true}})
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("turning a stored-off subscription back on, pinned but absent: refused", func(t *testing.T) {
		off := subscriptionProvider("claude-sub")
		off.Disabled = true
		srv, _ := signInImageSrv(t, types.SiteConfig{ModelProviders: providerBlock(off)}, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sign-in image") {
			t.Fatalf("PUT = %d, want 400 with the E4 sentence; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("an unrelated save carrying the stored subscription: accepted", func(t *testing.T) {
		stored := types.SiteConfig{ModelProviders: providerBlock(subscriptionProvider("claude-sub"))}
		srv, fake := signInImageSrv(t, stored, signInImageMissing())
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusOK || len(fake.putSeen.ScmHosts) != 1 {
			t.Fatalf("PUT = %d, want 200 with the save landed; body=%s", w.Code, w.Body.String())
		}
	})
}

// modelProvidersStatusSrv is a /setup/status server whose site config carries
// the given providers and roster, with the capability store a member reads.
func modelProvidersStatusSrv(t *testing.T, site types.SiteConfig, cs *capStore) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(cs), site: site})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	return New(cfg)
}

func statusModelProviders(t *testing.T, body []byte) []SetupModelProvider {
	t.Helper()
	var st struct {
		ModelProviders []SetupModelProvider `json:"model_providers"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	return st.ModelProviders
}

// TestSetupStatusModelProviders is the member-safe projection: the providers
// serving an agent this person may launch, the host their credential goes to
// (D7), which agents each is the default for — and none of the admin detail.
func TestSetupStatusModelProviders(t *testing.T) {
	sso := ssoProvider()
	sso.Bedrock.BaseURL = "https://vpce-1.bedrock-runtime.us-east-1.vpce.amazonaws.com"
	site := types.SiteConfig{
		ModelProviders: providerBlock(endpointProvider(), sso, keyProvider("anthropic", "claude-code"), keyProvider("unused")),
		AgentProviders: agentBlock(
			types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, DefaultProvider: "corp-gateway"},
			types.AgentProvider{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey},
		),
	}
	// Every member is denied Codex CLI.
	cs := &capStore{grants: []types.CapabilityGrant{{
		SubjectType: types.CapabilitySubjectAll, Capability: capAgent, Value: "codex-cli", Effect: types.CapabilityDeny,
	}}}
	srv := modelProvidersStatusSrv(t, site, cs)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /setup/status = %d; body=%s", w.Code, w.Body.String())
	}
	got := statusModelProviders(t, w.Body.Bytes())
	want := []SetupModelProvider{
		{ID: "corp-gateway", Name: "Corp gateway", Kind: "custom_endpoint", Harnesses: []string{"claude-code"},
			DefaultFor: []string{"claude-code"}, Host: "gateway.corp.example"},
		{ID: "bedrock-prod", Kind: "bedrock_sso", Harnesses: []string{"claude-code"},
			Host: "vpce-1.bedrock-runtime.us-east-1.vpce.amazonaws.com"},
		{ID: "anthropic", Kind: "anthropic_api_key", Harnesses: []string{"claude-code"}, Host: "api.anthropic.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("model_providers =\n%+v\nwant\n%+v", got, want)
	}
	for _, leak := range []string{"awsapps.com", "123456789012", "BedrockUser", "/anthropic", "Authorization", `"uid"`} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("the member's /setup/status carries %q", leak)
		}
	}

	t.Run("an admin reads the same shape, not denied Codex CLI", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
		got := statusModelProviders(t, w.Body.Bytes())
		if len(got) != 3 || !slices.Equal(got[0].Harnesses, []string{"claude-code", "codex-cli"}) {
			t.Errorf("admin model_providers = %+v, want corp-gateway on both agents", got)
		}
	})

	t.Run("an agent the roster turns off is not offered", func(t *testing.T) {
		off := site
		off.AgentProviders = agentBlock(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, Disabled: true})
		off.ModelProviders = providerBlock(keyProvider("anthropic", "claude-code"))
		w := do(t, modelProvidersStatusSrv(t, off, &capStore{}), http.MethodGet, "/api/v1/setup/status", adminToken, "")
		if strings.Contains(w.Body.String(), `"model_providers"`) {
			t.Errorf("a provider serving only a turned-off agent was offered: %s", w.Body.String())
		}
	})
}

// TestSetupStatusNilBlockIsToday is the golden for "additive only" on the one
// document every person reads: with no provider block there is no new key, for
// either tier, and the member body keeps exactly the keys it had.
func TestSetupStatusNilBlockIsToday(t *testing.T) {
	srv := modelProvidersStatusSrv(t, types.SiteConfig{}, &capStore{})
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	for name, body := range map[string]string{
		"admin":  do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "").Body.String(),
		"member": doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", member, "").Body.String(),
	} {
		if strings.Contains(body, "model_providers") {
			t.Errorf("%s /setup/status grew model_providers with no provider block: %s", name, body)
		}
	}
	var st map[string]any
	if err := json.Unmarshal(doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", member, "").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"age_key", "auth", "bedrock", "checks", "checks_redacted", "deployment", "harnesses", "has_runs", "host_proxy",
		"llm_ready", "onboarding_complete", "platform", "providers", "ready", "runner", "scm", "secrets",
	}
	if got := slices.Sorted(maps.Keys(st)); !slices.Equal(got, want) {
		t.Errorf("member /setup/status keys = %v\nwant today's %v", got, want)
	}
}

// TestRunCreateUnderAnUnservingProviderBlock pins the transition (#528): once
// a model-provider block is set, a model run of an agent no provider serves is
// on no legacy lane — it launches with the no-provider advisory, and a roster
// row's declared mechanism no longer refuses it. A block that does serve the
// agent chooses a provider (run_model_provider.go).
func TestRunCreateUnderAnUnservingProviderBlock(t *testing.T) {
	providers := providerBlock(keyProvider("codex", "codex-cli"))
	row := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}
	codex := types.AgentProvider{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey, DefaultProvider: "codex"}
	for _, tc := range []struct {
		name string
		site types.SiteConfig
	}{
		{"no roster", types.SiteConfig{ModelProviders: providers}},
		{"a roster declaring the legacy mechanism", types.SiteConfig{ModelProviders: providers, AgentProviders: agentBlock(row, codex)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: tc.site})
			cfg.Broker = h.broker
			cfg.Runner = &fakeRunner{}
			cfg.Secrets = &memSecrets{m: map[string][]byte{}}
			cfg.MaskRegistry = secretmask.NewRegistry()
			w := do(t, New(cfg), http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
			var got createRunResponse
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if w.Code != http.StatusCreated || !slices.Contains(got.Warnings, fmt.Sprintf(mpAccessNoProvider, "claude-code")) {
				t.Errorf("create = %d %s\nwant 201 carrying the no-provider advisory", w.Code, w.Body.String())
			}
		})
	}
}

func modelProviderWriteDatum(t *testing.T, audit *recRecorder) map[string]any {
	t.Helper()
	var d map[string]any
	for _, ev := range audit.events {
		if ev.Action == "model_provider.write" {
			d = nil
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d == nil {
		t.Fatal("no model_provider.write event")
	}
	return d
}
