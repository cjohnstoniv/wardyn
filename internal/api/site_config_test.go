// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 TestHandlePutSiteConfig_LegacyArtifactOverridesFold and
// TestHandlePutSiteConfig_RejectsBothArtifactOverridesAndEgressRedirects read
// the deprecated SiteConfig.ArtifactOverrides to prove the request-decode fold
// (foldLegacyArtifactOverrides) round-trips it correctly — see
// internal/api/site_config.go's own file-scope ignore for why the field still
// exists.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── validateSiteConfig ──────────────────────────────────────────────────────

func TestValidateSiteConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  types.SiteConfig
		ok   bool
	}{
		{"zero value is valid (unconfigured)", types.SiteConfig{}, true},
		{"good upstream proxy secret ref", types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}, true},
		{"bad upstream proxy secret ref (uppercase)", types.SiteConfig{UpstreamProxySecretRef: "Corp-Proxy"}, false},
		{"reserved upstream proxy secret ref", types.SiteConfig{UpstreamProxySecretRef: "wardyn-signing-key"}, false},
		{"good upstream proxy plain URL", types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, true},
		{"upstream proxy plain URL with embedded userinfo is REJECTED (Task 1's mandatory guard)",
			types.SiteConfig{UpstreamProxyURL: "http://user:pass@proxy.corp:3128"}, false},
		{"upstream proxy plain URL malformed", types.SiteConfig{UpstreamProxyURL: "not a url"}, false},
		{
			// W13-S1-4 regression: https:// used to pass validSiteURL (it accepts
			// both http/https for its OTHER callers) and save clean, then display
			// as the live chain while resolveUpstreamProxyURL silently dropped it
			// at dispatch (the sidecar's plaintext-CONNECT hop cannot carry
			// https). Must be rejected at the SAME gate dispatch applies.
			"upstream proxy plain URL https is REJECTED (dispatch cannot use it — W13-S1-4)",
			types.SiteConfig{UpstreamProxyURL: "https://proxy.corp:8443"}, false,
		},
		{"good scm host", types.SiteConfig{ScmHosts: []string{"dev.azure.com"}}, true},
		{"scm host with scheme", types.SiteConfig{ScmHosts: []string{"https://dev.azure.com"}}, false},
		{"scm host with port", types.SiteConfig{ScmHosts: []string{"dev.azure.com:443"}}, false},
		{"scm host wildcard", types.SiteConfig{ScmHosts: []string{"*.azure.com"}}, false},
		{"scm host no dot", types.SiteConfig{ScmHosts: []string{"localhost"}}, false},
		{
			"good ecosystem egress redirect", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token", Ecosystem: "npm"},
			}}, true,
		},
		{
			"good network-only egress redirect (no ecosystem)", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-remote"},
			}}, true,
		},
		{
			"unknown ecosystem", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://x.corp/gems/", To: "https://artifactory.corp/api/gems/gems-remote/", Ecosystem: "rubygems"},
			}}, false,
		},
		{
			"bad scheme (ftp) in to", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "ftp://artifactory.corp/npm/", Ecosystem: "npm"},
			}}, false,
		},
		{
			"bad from (shell metacharacters)", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/`whoami`", To: "https://artifactory.corp/npm/", Ecosystem: "npm"},
			}}, false,
		},
		{
			"control char in to", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm/\n", Ecosystem: "npm"},
			}}, false,
		},
		{
			"bad token secret ref", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "Bad Ref!", Ecosystem: "npm"},
			}}, false,
		},
		{
			"reserved token secret ref", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "wardyn-session-key", Ecosystem: "npm"},
			}}, false,
		},
		{
			"private IP host is still a well-formed URL (host validation, not SSRF IP-block)",
			types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "https://registry.npmjs.org/", To: "https://10.0.0.5/npm/", Ecosystem: "npm"},
			}}, true,
		},
		{
			"good internal host, no cidrs (full liftable set)",
			types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}, true,
		},
		{
			"good internal host, cidr inside RFC1918",
			types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}}}, true,
		},
		{
			"bad internal host suffix (scheme)",
			types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "https://corp.internal"}}}, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSiteConfig(c.cfg)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// TestValidateSiteConfig_InternalHosts_Rejects: every declared CIDR must lie
// ENTIRELY inside ipguard.Liftable (RFC1918/fc00::/7/100.64.0.0/10) — the
// obvious SSRF-guard-widening mistakes are all refused at write time.
func TestValidateSiteConfig_InternalHosts_Rejects(t *testing.T) {
	bad := []string{
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local/metadata
		"0.0.0.0/0",      // everything
		"8.8.8.0/24",     // public
		"::/0",           // everything, v6
	}
	for _, cidr := range bad {
		t.Run(cidr, func(t *testing.T) {
			err := validateInternalHosts([]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{cidr}}})
			if err == nil {
				t.Fatalf("cidr %q must be rejected (outside ipguard.Liftable)", cidr)
			}
		})
	}
}

// ─── handler tests ───────────────────────────────────────────────────────────

// fakeSiteConfigStore is a minimal store.Store for the site-config handlers.
type fakeSiteConfigStore struct {
	store.Store
	cfg     types.SiteConfig
	getErr  error
	putErr  error
	putSeen *types.SiteConfig
}

func (s *fakeSiteConfigStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	if s.getErr != nil {
		return types.SiteConfig{}, s.getErr
	}
	return s.cfg, nil
}

func (s *fakeSiteConfigStore) PutSiteConfig(_ context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	if s.putErr != nil {
		return types.SiteConfig{}, s.putErr
	}
	s.putSeen = &cfg
	s.cfg = cfg
	return cfg, nil
}

func newSiteConfigHarness(t *testing.T, fake *fakeSiteConfigStore) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	return New(baseTestConfig(h, fake)), h.audit
}

func TestHandleGetSiteConfig_Unconfigured(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, types.SiteConfig{}) {
		t.Errorf("expected zero-value config, got %+v", got)
	}
}

func TestHandlePutSiteConfig_ValidationRejected(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	cases := []struct {
		name, body string
	}{
		{"invalid json", `{not json`},
		{"unknown field", `{"upstream_proxy_secret_ref":"x","bogus":1}`},
		{"bad secret ref", `{"upstream_proxy_secret_ref":"Bad Ref"}`},
		{"reserved secret ref", `{"upstream_proxy_secret_ref":"wardyn-signing-key"}`},
		{"upstream proxy url with embedded userinfo", `{"upstream_proxy_url":"http://user:pass@proxy.corp:3128"}`},
		{"bad scm host", `{"scm_hosts":["https://dev.azure.com"]}`},
		{"unknown ecosystem", `{"egress_redirects":[{"from":"https://x.corp/gems/","to":"https://artifactory.corp/api/gems/gems-remote/","ecosystem":"rubygems"}]}`},
		{"bad to url scheme", `{"egress_redirects":[{"from":"https://registry.npmjs.org/","to":"ftp://x.corp/npm/","ecosystem":"npm"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, c.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
	if fake.putSeen != nil {
		t.Errorf("a rejected write must never reach the store, got %+v", fake.putSeen)
	}
	if len(audit.events) != 0 {
		t.Errorf("a rejected write must not audit, got %d events", len(audit.events))
	}
}

// TestHandlePutSiteConfig_RoundTripAndAudit is the audit-on-write assertion the
// plan calls for: a valid PUT persists via the store AND emits exactly one
// site_config.write audit event (mirroring secret.write), and the response
// never contains a secret VALUE (only the refs it was given).
func TestHandlePutSiteConfig_RoundTripAndAudit(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)

	body := `{
		"upstream_proxy_secret_ref": "corp-proxy-url",
		"egress_redirects": [
			{"from": "https://registry.npmjs.org/", "to": "https://artifactory.corp/api/npm/npm-remote/", "token_secret_ref": "npm-token", "ecosystem": "npm"}
		],
		"scm_hosts": ["dev.azure.com"]
	}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UpstreamProxySecretRef != "corp-proxy-url" {
		t.Errorf("UpstreamProxySecretRef = %q, want corp-proxy-url", got.UpstreamProxySecretRef)
	}
	if fake.putSeen == nil {
		t.Fatal("valid write did not reach the store")
	}

	// Audit: exactly one site_config.write, success outcome.
	var writes []types.AuditEvent
	for _, ev := range audit.events {
		if ev.Action == "site_config.write" {
			writes = append(writes, ev)
		}
	}
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 site_config.write audit event, got %d: %+v", len(writes), audit.events)
	}
	if writes[0].Outcome != "success" {
		t.Errorf("audit outcome = %q, want success", writes[0].Outcome)
	}

	// GET after PUT reflects the persisted refs (never a secret value: the
	// fixture only ever stored the ref string, so this also proves the response
	// path never widens a ref into a value).
	w2 := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200", w2.Code)
	}
	var got2 types.SiteConfig
	if err := json.Unmarshal(w2.Body.Bytes(), &got2); err != nil {
		t.Fatal(err)
	}
	if len(got2.EgressRedirects) != 1 || got2.EgressRedirects[0].TokenSecretRef != "npm-token" {
		t.Errorf("GET EgressRedirects = %+v, want one npm entry with TokenSecretRef npm-token", got2.EgressRedirects)
	}
}

// TestHandlePutSiteConfig_ReportsDanglingSecretRefs pins W26-S1-2: PUT
// /site-config must surface, never silently accept, a secret ref the store
// doesn't currently hold (e.g. `wardyn site-config apply corp-baseline.json`
// run before the referenced secrets were restored). The write itself still
// succeeds — dangling is a valid mid-recovery state, never rejected.
func TestHandlePutSiteConfig_ReportsDanglingSecretRefs(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	// Only npm-token is present; corp-proxy-url is dangling.
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"npm-token": []byte("tok")}}
	srv.router = srv.routes() // re-mount with the secret surfaces enabled

	body := `{
		"upstream_proxy_secret_ref": "corp-proxy-url",
		"egress_redirects": [
			{"from": "https://registry.npmjs.org/", "to": "https://artifactory.corp/api/npm/npm-remote/", "token_secret_ref": "npm-token", "ecosystem": "npm"}
		]
	}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (dangling refs are advisory, not rejected); body=%s", w.Code, w.Body.String())
	}
	var got struct {
		DanglingSecretRefs []string `json:"dangling_secret_refs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.DanglingSecretRefs) != 1 || got.DanglingSecretRefs[0] != "corp-proxy-url" {
		t.Errorf("dangling_secret_refs = %v, want [corp-proxy-url] (npm-token is present, must not be listed)", got.DanglingSecretRefs)
	}
	if fake.putSeen == nil || fake.putSeen.UpstreamProxySecretRef != "corp-proxy-url" {
		t.Fatal("a dangling ref must still be persisted as given, never rejected")
	}
}

// TestHandlePutSiteConfig_LegacyArtifactOverridesFold is the fold-compat proof
// for Task 3: a body saved before EgressRedirects existed (still keyed by the
// deprecated artifact_overrides) must keep applying — folded into
// EgressRedirects with the correct per-ecosystem From URL, never persisted back
// in the old shape.
func TestHandlePutSiteConfig_LegacyArtifactOverridesFold(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)

	body := `{"artifact_overrides": {
		"npm": {"base_url": "https://artifactory.corp/api/npm/npm-remote/", "token_secret_ref": "npm-token"},
		"go":  {"base_url": "https://artifactory.corp/api/go/go-remote"}
	}}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ArtifactOverrides) != 0 {
		t.Errorf("ArtifactOverrides = %+v, want empty (folded, never persisted in the old shape)", got.ArtifactOverrides)
	}
	if len(got.EgressRedirects) != 2 {
		t.Fatalf("EgressRedirects = %+v, want 2 folded entries", got.EgressRedirects)
	}
	// Sorted by ecosystem key (go < npm) -- see foldLegacyArtifactOverrides.
	if got.EgressRedirects[0].Ecosystem != "go" || got.EgressRedirects[0].From != "https://proxy.golang.org" ||
		got.EgressRedirects[0].To != "https://artifactory.corp/api/go/go-remote" || got.EgressRedirects[0].TokenSecretRef != "" {
		t.Errorf("EgressRedirects[0] = %+v, want the folded go entry", got.EgressRedirects[0])
	}
	if got.EgressRedirects[1].Ecosystem != "npm" || got.EgressRedirects[1].From != "https://registry.npmjs.org/" ||
		got.EgressRedirects[1].To != "https://artifactory.corp/api/npm/npm-remote/" || got.EgressRedirects[1].TokenSecretRef != "npm-token" {
		t.Errorf("EgressRedirects[1] = %+v, want the folded npm entry", got.EgressRedirects[1])
	}
	// The STORE must never see the legacy field either — only the fold's output.
	if fake.putSeen == nil || len(fake.putSeen.ArtifactOverrides) != 0 {
		t.Errorf("store received ArtifactOverrides = %+v, want empty (fold must clear it before persisting)", fake.putSeen)
	}
}

// TestHandlePutSiteConfig_RejectsBothArtifactOverridesAndEgressRedirects: a
// body that sets BOTH the deprecated and the current field is ambiguous (which
// one is authoritative?) and must 400 rather than silently pick one.
func TestHandlePutSiteConfig_RejectsBothArtifactOverridesAndEgressRedirects(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	body := `{
		"artifact_overrides": {"npm": {"base_url": "https://artifactory.corp/npm/"}},
		"egress_redirects": [{"from": "https://registry.npmjs.org/", "to": "https://artifactory.corp/npm/", "ecosystem": "npm"}]
	}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Errorf("a rejected write must never reach the store, got %+v", fake.putSeen)
	}
	if len(audit.events) != 0 {
		t.Errorf("a rejected write must not audit, got %d events", len(audit.events))
	}
}

// TestUnionSiteConfigScmHosts asserts unionSiteConfigScmHosts adds the
// operator's declared ScmHosts (deduped against what's already allowed),
// no-ops when unconfigured/errored, and never touches AllowedDomains it
// didn't add (additive only).
func TestUnionSiteConfigScmHosts(t *testing.T) {
	ctx := context.Background()

	t.Run("adds missing hosts, dedupes existing", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"ghes.corp.internal", "github.com"}}}
		srv, _ := newSiteConfigHarness(t, fake)
		spec := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}

		added := srv.unionSiteConfigScmHosts(ctx, &spec)
		if strings.Join(added, ",") != "ghes.corp.internal" {
			t.Errorf("added = %v, want [ghes.corp.internal] (github.com already present)", added)
		}
		if strings.Join(spec.AllowedDomains, ",") != "github.com,ghes.corp.internal" {
			t.Errorf("AllowedDomains = %v", spec.AllowedDomains)
		}
	})

	t.Run("no site config configured is a no-op", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, _ := newSiteConfigHarness(t, fake)
		spec := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}

		if added := srv.unionSiteConfigScmHosts(ctx, &spec); added != nil {
			t.Errorf("added = %v, want nil", added)
		}
		if strings.Join(spec.AllowedDomains, ",") != "github.com" {
			t.Errorf("AllowedDomains mutated: %v", spec.AllowedDomains)
		}
	})

	t.Run("store error is a no-op, never fatal", func(t *testing.T) {
		fake := &fakeSiteConfigStore{getErr: context.DeadlineExceeded}
		srv, _ := newSiteConfigHarness(t, fake)
		spec := types.RunPolicySpec{}

		if added := srv.unionSiteConfigScmHosts(ctx, &spec); added != nil {
			t.Errorf("added = %v, want nil", added)
		}
	})

	t.Run("nil Store is a no-op", func(t *testing.T) {
		srv := New(Config{TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080"})
		spec := types.RunPolicySpec{}
		if added := srv.unionSiteConfigScmHosts(ctx, &spec); added != nil {
			t.Errorf("added = %v, want nil", added)
		}
	})
}

func TestHandleGetSiteConfig_StoreError(t *testing.T) {
	fake := &fakeSiteConfigStore{getErr: context.DeadlineExceeded}
	srv, _ := newSiteConfigHarness(t, fake)
	w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// ─── the integrations clobber guard ──────────────────────────────────────────
//
// PUT /site-config replaces the whole document. An older client that GETs a
// config written before `integrations` existed, then PUTs it back, would
// silently DELETE every stored integration if this guard were missing.

// A request body carrying a non-empty integrations is rejected outright: they
// are managed through their own endpoints, never through this one.
func TestHandlePutSiteConfig_RejectsIntegrations(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	body := `{"integrations":[{"id":"x","name":"X","category":"ai_provider","type":"anthropic_api_key"}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Errorf("a rejected write must never reach the store, got %+v", fake.putSeen)
	}
	if len(audit.events) != 0 {
		t.Errorf("a rejected write must not audit, got %d events", len(audit.events))
	}
}

// A PUT that omits integrations (the common case for any client, old or new)
// must carry the STORED integrations forward verbatim rather than wiping them.
func TestHandlePutSiteConfig_CarriesStoredIntegrationsForward(t *testing.T) {
	existing := types.Integration{
		ID: "x", Name: "X", Kind: types.IntegrationKindAnthropicAPIKey,
	}
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{existing}}}
	srv, _ := newSiteConfigHarness(t, fake)

	body := `{"upstream_proxy_secret_ref": "corp-proxy-url"}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Integrations) != 1 || got.Integrations[0].ID != "x" {
		t.Errorf("response Integrations = %+v, want the stored [x] carried forward", got.Integrations)
	}
	if fake.putSeen == nil || len(fake.putSeen.Integrations) != 1 || fake.putSeen.Integrations[0].ID != "x" {
		t.Fatalf("store did not receive the carried-forward integrations: %+v", fake.putSeen)
	}
	// The rest of THIS request's write still landed — carrying integrations
	// forward must not clobber anything else.
	if fake.putSeen.UpstreamProxySecretRef != "corp-proxy-url" {
		t.Errorf("UpstreamProxySecretRef = %q, want corp-proxy-url", fake.putSeen.UpstreamProxySecretRef)
	}
}

// A failure reading the existing config before carrying its integrations
// forward must fail the whole PUT (500), never silently proceed with an
// empty integrations list — that would reintroduce the exact data loss this
// guard exists to prevent.
func TestHandlePutSiteConfig_GetExistingErrorFailsClosed(t *testing.T) {
	fake := &fakeSiteConfigStore{getErr: context.DeadlineExceeded}
	srv, _ := newSiteConfigHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"upstream_proxy_secret_ref":"corp-proxy-url"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (fail closed rather than risk wiping stored integrations); body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Errorf("a failed carry-forward read must never reach PutSiteConfig, got %+v", fake.putSeen)
	}
}

// doIfMatch is `do` (api_test.go) plus an If-Match header — the one thing
// none of this package's request helpers carry, and the only thing this
// test needs beyond them.
func doIfMatch(t *testing.T, srv *Server, method, path, bearer, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	r.Host = "127.0.0.1"             // see do (api_test.go) FIX #8
	r.RemoteAddr = "127.0.0.1:54321" // see do (api_test.go) N1
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestHandlePutSiteConfig_IfMatch is the #7 optimistic-concurrency contract:
// no If-Match keeps working (unchanged behavior), a stale one 412s BEFORE the
// store is touched, and a fresh one (or none) succeeds and returns a new
// ETag reflecting what was just written.
func TestHandlePutSiteConfig_IfMatch(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"dev.azure.com"}}}
	srv, _ := newSiteConfigHarness(t, fake)

	get := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET /site-config did not set an ETag")
	}

	// A stale If-Match (the document has since changed underneath it) is
	// refused before the write reaches the store.
	stale := `"0000000000000000000000000000000000000000000000000000000000000000"`
	w := doIfMatch(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, stale, `{"scm_hosts":["gitlab.corp"]}`)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: code = %d, want 412; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Fatalf("a refused If-Match must never reach the store, got %+v", fake.putSeen)
	}

	// The FRESH ETag from the GET above satisfies If-Match and the write
	// proceeds, returning a new ETag for what was just persisted.
	w = doIfMatch(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, etag, `{"scm_hosts":["gitlab.corp"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("fresh If-Match: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || len(fake.putSeen.ScmHosts) != 1 || fake.putSeen.ScmHosts[0] != "gitlab.corp" {
		t.Fatalf("fresh If-Match write did not reach the store as given: %+v", fake.putSeen)
	}
	if got := w.Header().Get("ETag"); got == "" || got == etag {
		t.Fatalf("PUT ETag = %q, want a NEW value distinct from the pre-write one %q", got, etag)
	}

	// No If-Match at all: unconditional, exactly as before this feature.
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.com"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("no If-Match: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
