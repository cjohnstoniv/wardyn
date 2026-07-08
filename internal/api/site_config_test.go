// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
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
		{"good scm host", types.SiteConfig{ScmHosts: []string{"dev.azure.com"}}, true},
		{"scm host with scheme", types.SiteConfig{ScmHosts: []string{"https://dev.azure.com"}}, false},
		{"scm host with port", types.SiteConfig{ScmHosts: []string{"dev.azure.com:443"}}, false},
		{"scm host wildcard", types.SiteConfig{ScmHosts: []string{"*.azure.com"}}, false},
		{"scm host no dot", types.SiteConfig{ScmHosts: []string{"localhost"}}, false},
		{
			"good artifact override", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token"},
			}}, true,
		},
		{
			"unknown ecosystem", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"rubygems": {BaseURL: "https://artifactory.corp/api/gems/gems-remote/"},
			}}, false,
		},
		{
			"bad scheme (ftp)", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "ftp://artifactory.corp/npm/"},
			}}, false,
		},
		{
			"no scheme", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "artifactory.corp/npm/"},
			}}, false,
		},
		{
			"shell metacharacters", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://artifactory.corp/npm/`whoami`"},
			}}, false,
		},
		{
			"control char", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://artifactory.corp/npm/\n"},
			}}, false,
		},
		{
			"bad token secret ref", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "Bad Ref!"},
			}}, false,
		},
		{
			"reserved token secret ref", types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "wardyn-session-key"},
			}}, false,
		},
		{
			"private IP host is still a well-formed URL (host validation, not SSRF IP-block)",
			types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
				"npm": {BaseURL: "https://10.0.0.5/npm/"},
			}}, true,
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
	srv := New(Config{
		Identity:        h.idp,
		Audit:           h.audit,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
		Store:           fake,
	})
	return srv, h.audit
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
		{"bad scm host", `{"scm_hosts":["https://dev.azure.com"]}`},
		{"unknown ecosystem", `{"artifact_overrides":{"rubygems":{"base_url":"https://x.corp/gems/"}}}`},
		{"bad base url scheme", `{"artifact_overrides":{"npm":{"base_url":"ftp://x.corp/npm/"}}}`},
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
		"artifact_overrides": {
			"npm": {"base_url": "https://artifactory.corp/api/npm/npm-remote/", "token_secret_ref": "npm-token"}
		},
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
	if got2.ArtifactOverrides["npm"].TokenSecretRef != "npm-token" {
		t.Errorf("GET ArtifactOverrides[npm].TokenSecretRef = %q, want npm-token", got2.ArtifactOverrides["npm"].TokenSecretRef)
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
