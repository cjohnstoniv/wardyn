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
	"fmt"
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
			// F3-F5 (server half): two rows sharing a From used to save fine —
			// findEgressRedirect resolves the first match only, so the second
			// row was a silent dead entry.
			"duplicate from is rejected", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-remote"},
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-mirror-2"},
			}}, false,
		},
		{
			// Case differs, same host: EqualFold-equivalent — findEgressRedirect
			// would still resolve only the first at read time.
			"duplicate from, different case is still rejected", types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "GHCR.io", To: "registry.corp.internal/ghcr-remote"},
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-mirror-2"},
			}}, false,
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

// TestValidateSiteConfig_DuplicateFromMessage asserts THROUGH the
// egressRedirectDuplicateFromRefusal DRAFT constant (F3-F5, server half).
func TestValidateSiteConfig_DuplicateFromMessage(t *testing.T) {
	err := validateSiteConfig(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "ghcr.io", To: "registry.corp.internal/ghcr-remote"},
		{From: "ghcr.io", To: "registry.corp.internal/ghcr-mirror-2"},
	}})
	if want := fmt.Sprintf(egressRedirectDuplicateFromRefusal, 1, "ghcr.io", 0); err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// TestShellSafeSiteString_IsTheOneInjectionGate pins the property
// shellSafeSiteString exists to make structural: every site-config string that
// reaches this gate is refused on the SAME control characters, DEL, shell/XML
// metacharacters and over-long inputs — including validSiteURLOrHost's third
// shape (a bare host with a port/path and no scheme), which no scheme or host
// check rejects on its own and which TestValidateSiteConfig never exercised
// (its metacharacter case carries a scheme, so the "://" arm refuses it first).
// hostrules.EmitArtifactConfig interpolates these strings verbatim into
// .npmrc/pip.conf/.cargo/config.toml/settings.xml/NuGet.Config and names
// validateSiteConfig as the reason that is safe, so the gate is the whole
// justification for that interpolation.
func TestShellSafeSiteString_IsTheOneInjectionGate(t *testing.T) {
	unsafe := []struct{ name, raw string }{
		{"backtick", "registry.corp.internal/`whoami`"},
		{"dollar", "registry.corp.internal/$(id)"},
		{"semicolon", "registry.corp.internal/x;id"},
		{"ampersand", "registry.corp.internal/x&id"},
		{"pipe", "registry.corp.internal/x|id"},
		{"angle brackets", "registry.corp.internal/<x>"},
		{"double quote", `registry.corp.internal/"x"`},
		{"single quote", "registry.corp.internal/'x'"},
		{"backslash", `registry.corp.internal\x`},
		{"newline", "registry.corp.internal/x\n"},
		{"NUL", "registry.corp.internal/x\x00"},
		{"DEL", "registry.corp.internal/x\x7f"},
		{"empty", ""},
		{"over 2048 bytes", "registry.corp.internal/" + strings.Repeat("a", 2048)},
	}
	for _, c := range unsafe {
		t.Run(c.name, func(t *testing.T) {
			if shellSafeSiteString(c.raw) {
				t.Errorf("shellSafeSiteString(%q) = true, want false", c.raw)
			}
			// Both callers of the gate, so deleting the call from either one
			// (the state before the extraction: two hand-kept copies) is red.
			if validSiteURLOrHost(c.raw) {
				t.Errorf("validSiteURLOrHost(%q) = true, want false", c.raw)
			}
			if validSiteURL("https://" + c.raw) {
				t.Errorf("validSiteURL(%q) = true, want false", "https://"+c.raw)
			}
			// End to end: the refusal validateSiteConfig actually owes an
			// operator, on the network-only tier where a bare host is legal.
			if err := validateSiteConfig(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "ghcr.io", To: c.raw},
			}}); err == nil {
				t.Errorf("validateSiteConfig accepted a network-only redirect to %q", c.raw)
			}
		})
	}
	// ...and the gate is not a blanket refusal of the third shape: the two
	// documented examples still pass.
	for _, ok := range []string{"registry.corp.internal/ghcr-remote", "10.40.2.11:8443"} {
		if !validSiteURLOrHost(ok) {
			t.Errorf("validSiteURLOrHost(%q) = false, want true (documented third-shape example)", ok)
		}
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

// TestValidateSiteConfig_InternalHosts_Accepts is the other half of the
// validator's contract, and the half a refusal-only table cannot pin: a floor
// that refused RFC1918 or CGNAT would refuse the whole feature (a PrivateLink
// endpoint and an in-cluster service both live there), so the ACCEPT rows are
// load-bearing. No CIDRs at all is valid too — it lifts the full Liftable set.
//
// The last subtest is the one worth having: `::ffff:10.0.0.0/104` is the
// v6-mapped spelling of 10.0.0.0/8, and it is REFUSED. netip.Prefix.Contains
// never matches a v4-mapped v6 address against a v4 prefix, so a v4-mapped
// declaration falls outside Liftable and the validator fails CLOSED — the safe
// direction, but previously unasserted, i.e. a normalization "fix" could
// silently start widening the guard with nothing to catch it.
func TestValidateSiteConfig_InternalHosts_Accepts(t *testing.T) {
	good := [][]string{
		nil,               // no CIDRs: lifts the full Liftable set
		{"100.64.0.0/10"}, // CGNAT — what an AWS PrivateLink endpoint resolves into
		{"10.40.0.0/16"},  // RFC1918 subset
		{"fc00::/7"},      // IPv6 ULA, the whole range
		{"10.40.0.0/16", "fd00::/8"},
	}
	for _, cidrs := range good {
		t.Run(strings.Join(cidrs, ","), func(t *testing.T) {
			if err := validateInternalHosts([]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: cidrs}}); err != nil {
				t.Fatalf("cidrs %v must be accepted (inside ipguard.Liftable): %v", cidrs, err)
			}
		})
	}
	t.Run("v4-mapped ::ffff:10.0.0.0/104 refused (fail closed)", func(t *testing.T) {
		if err := validateInternalHosts([]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"::ffff:10.0.0.0/104"}}}); err == nil {
			t.Fatal("the v4-mapped spelling of 10.0.0.0/8 must be refused: Liftable holds v4 prefixes and netip never matches a v4-mapped v6 address against one, so accepting it would authorize a lift the guard cannot actually reason about")
		}
	})
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

func (s *fakeSiteConfigStore) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return map[string]map[string]bool{}, nil
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
		{"F3-F5: duplicate from", `{"egress_redirects":[
			{"from":"ghcr.io","to":"registry.corp.internal/ghcr-remote"},
			{"from":"ghcr.io","to":"registry.corp.internal/ghcr-mirror-2"}
		]}`},
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

	// applies_from (B2): a site-config change does NOT reach a run already
	// going — the egress sidecar loads its compiled config once at sandbox
	// start. That lifetime was real and undocumented, and a customer read a 403
	// naming a field they had just fixed and retried the same run ten times. The
	// value is a fixed word so the console can switch on it.
	if !strings.Contains(w.Body.String(), `"applies_from":"next_dispatch"`) {
		t.Errorf("PUT response carries no applies_from: %s", w.Body.String())
	}
	var putResp siteConfigPutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &putResp); err != nil {
		t.Fatal(err)
	}
	if putResp.AppliesFrom != siteConfigAppliesFromNextDispatch {
		t.Errorf("applies_from = %q, want the siteConfigAppliesFromNextDispatch constant", putResp.AppliesFrom)
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

// TestHandlePutSiteConfig_LegacyArtifactOverridesUnknownEcosystem is B7-F9:
// an unknown ecosystem key used to resolve to ecosystemPublicURL[eco] == "",
// which the fold happily emitted as an EgressRedirect with From="" —
// validateSiteConfig's NEXT pass then 400ed it as `egress_redirects[0]:
// invalid from ""`, never naming the actual offending artifact_overrides key,
// and never reaching the "unknown ecosystem" message that exists for exactly
// this case two guards further down (it validates EgressRedirects, which by
// then never carries the raw legacy key). The fold itself must refuse it,
// naming `artifact_overrides.<key>`.
func TestHandlePutSiteConfig_LegacyArtifactOverridesUnknownEcosystem(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)

	body := `{"artifact_overrides": {"rubygems": {"base_url": "https://artifactory.corp/api/gems/gems-remote/"}}}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if want := fmt.Sprintf(legacyArtifactOverridesUnknownEcosystemRefusal, "rubygems"); !strings.Contains(w.Body.String(), want) {
		t.Errorf("body = %s, want it to contain the DRAFT constant %q", w.Body.String(), want)
	}
	// Not the empty-From message the same unknown key used to 400 as instead.
	if strings.Contains(w.Body.String(), `invalid from ""`) {
		t.Errorf("body = %s, still surfaces the useless empty-From message instead of naming the key", w.Body.String())
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
// no-ops when unconfigured, and never touches AllowedDomains it didn't add
// (additive only). It reads nothing: what happens when site config cannot be
// read is scmLaneSiteConfig's answer, pinned by
// TestAutonomySiteConfigReadFailureFailsClosed.
func TestUnionSiteConfigScmHosts(t *testing.T) {
	t.Run("adds missing hosts, dedupes existing", func(t *testing.T) {
		spec := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}

		added := unionSiteConfigScmHosts(&spec, types.SiteConfig{ScmHosts: []string{"ghes.corp.internal", "github.com"}})
		if strings.Join(added, ",") != "ghes.corp.internal" {
			t.Errorf("added = %v, want [ghes.corp.internal] (github.com already present)", added)
		}
		if strings.Join(spec.AllowedDomains, ",") != "github.com,ghes.corp.internal" {
			t.Errorf("AllowedDomains = %v", spec.AllowedDomains)
		}
	})

	t.Run("no site config configured is a no-op", func(t *testing.T) {
		spec := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}

		if added := unionSiteConfigScmHosts(&spec, types.SiteConfig{}); added != nil {
			t.Errorf("added = %v, want nil", added)
		}
		if strings.Join(spec.AllowedDomains, ",") != "github.com" {
			t.Errorf("AllowedDomains mutated: %v", spec.AllowedDomains)
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
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
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

// TestValidateSiteConfig_RedirectEndpointPort is Requirement 11's port half:
// two parsers must never disagree about one stored string, so the disagreement
// is refused at the write instead of resolved differently by each reader.
// Nothing in the PUT path examined a redirect endpoint's port before this —
// validSiteURLOrHost bottoms out in hostrules.HostOf, which discards everything
// from the first ':' onward — so ":0", ":99999", ":-1" and a query glued to the
// authority all saved with 200 OK. Downstream, redirectPort coerced three of
// them to 443 (mis-scoping plan.mitmHosts, i.e. WHERE the operator's registry
// token is injected, and making the redirect probe dial a port the operator
// never configured) while url.Parse refused the fourth, dropping the probe's
// --connect-to swap into the false-"blocked" it exists to prevent.
func TestValidateSiteConfig_RedirectEndpointPort(t *testing.T) {
	bad := []string{"10.40.2.11:0", "10.40.2.11:99999", "10.40.2.11:-1",
		"https://10.40.2.11:0", "mirror.corp.example:0", "mirror.corp.example:65536"}
	for _, to := range bad {
		cfg := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
			{From: "registry.npmjs.org", To: to},
		}}
		err := validateSiteConfig(cfg)
		if err == nil {
			t.Errorf("to %q was accepted: redirectPort then silently reads it as %d, a port the operator never configured",
				to, redirectPort(to))
			continue
		}
		if !strings.Contains(err.Error(), "egress_redirects[0]") {
			t.Errorf("to %q: error %q must name the offending index", to, err)
		}
	}
	// From is validated by the same rule — it is the authority the probe's
	// PORT1 and the run's egress host both come from.
	if err := validateSiteConfig(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.npmjs.org:0", To: "10.40.2.11:8443"},
	}}); err == nil {
		t.Error("from \"registry.npmjs.org:0\" was accepted; the port must be validated on BOTH endpoints")
	}
	// The shapes that must keep saving: a real port, no port, and a path.
	for _, to := range []string{"10.40.2.11:8443", "10.40.2.11", "mirror.corp.example:8443",
		"https://mirror.corp.example:8443/artifactory/api/npm/npm-remote", "registry.corp.internal/ghcr-remote"} {
		if err := validateSiteConfig(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
			{From: "registry.npmjs.org", To: to},
		}}); err != nil {
			t.Errorf("to %q must still save: %v", to, err)
		}
	}
}

// TestHandlePutSiteConfig_NormalizesTopologyToCanonicalForm is B7-F8:
// ScmHosts/EgressRedirects[].{From,To}/UpstreamProxyURL used to save whatever
// case/whitespace the operator typed — validSiteHost/HostOf only trim+lower a
// THROWAWAY copy to check it, never the stored string — so findEgressRedirect's
// read-time EqualFold masked the effect for that one lookup while the document
// itself, and everything that echoes it (GET, `wardyn site-config get`, the
// audit datum), stayed uncanonicalized. Interior whitespace is a 400, not a
// silent collapse — see normalizeSiteConfigTopology's doc.
func TestHandlePutSiteConfig_NormalizesTopologyToCanonicalForm(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)

	body := `{
		"upstream_proxy_url": "  HTTP://Proxy.Corp.Example:3128  ",
		"scm_hosts": ["  Dev.Azure.COM  "],
		"egress_redirects": [
			{"from": "  Registry.NPMJS.org  ", "to": "Artifactory.Corp.Internal/NPM-Remote"}
		]
	}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.SiteConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UpstreamProxyURL != "http://proxy.corp.example:3128" {
		t.Errorf("UpstreamProxyURL = %q, want the canonical lowercase form", got.UpstreamProxyURL)
	}
	if len(got.ScmHosts) != 1 || got.ScmHosts[0] != "dev.azure.com" {
		t.Errorf("ScmHosts = %v, want [dev.azure.com]", got.ScmHosts)
	}
	if len(got.EgressRedirects) != 1 {
		t.Fatalf("EgressRedirects = %v, want one row", got.EgressRedirects)
	}
	red := got.EgressRedirects[0]
	if red.From != "registry.npmjs.org" {
		t.Errorf("From = %q, want the canonical lowercase host (no outer whitespace)", red.From)
	}
	// The path segment ("/NPM-Remote") is left exactly as typed — only the
	// authority folds, per normalizeRedirectEndpoint's doc.
	if red.To != "artifactory.corp.internal/NPM-Remote" {
		t.Errorf("To = %q, want the authority lowered and the path untouched", red.To)
	}
	// findEgressRedirect must still resolve the row from a DIFFERENT case than
	// either the operator typed or the server stored — the read-time EqualFold
	// this pins never depended on the stored casing in the first place.
	if _, ok := findEgressRedirect(got, "REGISTRY.NPMJS.ORG"); !ok {
		t.Errorf("findEgressRedirect could not resolve the stored row by a third casing")
	}

	// Interior whitespace is refused, not silently collapsed into one token.
	w2 := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"scm_hosts": ["git .corp.example"]}`)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("scm_hosts with interior whitespace = %d, want 400; body=%s", w2.Code, w2.Body.String())
	}
}

// TestHandlePutSiteConfig_AuditDatumCarriesTopologyNotSecrets is B7-F4: the
// datum used to answer ONLY upstream_proxy_configured (a bool) — two PUTs
// naming two DIFFERENT proxy URLs produced the IDENTICAL audit row, so a
// review could tell THAT the proxy changed but never TO WHAT, nor what the
// org's egress redirects or internal-host allowlist actually route (an
// MDM-applied narrowing/opening was unreviewable from the log alone, the
// same gap workspace_provider.write's base_urls already closed for git
// providers). Fixed by recording the topology in the clear — precedent:
// workspace_provider.write's own base_urls doc, "topology, not a
// credential" — while keeping every actual secret VALUE out of the row:
// only ref NAMES (upstream_proxy_secret_ref) ever appear.
func TestHandlePutSiteConfig_AuditDatumCarriesTopologyNotSecrets(t *testing.T) {
	const secretValue = "corp-proxy-basic-auth-password-must-never-leak"
	fake := &fakeSiteConfigStore{}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.Secrets = &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte(secretValue)}}
	srv := New(cfg)

	put := func(body string) map[string]any {
		t.Helper()
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var last *types.AuditEvent
		for i, ev := range h.audit.events {
			if ev.Action == "site_config.write" {
				last = &h.audit.events[i]
			}
		}
		if last == nil {
			t.Fatalf("no site_config.write audit event; events=%+v", h.audit.events)
		}
		if strings.Contains(string(last.Data), secretValue) {
			t.Fatalf("audit datum leaked the secret VALUE: %s", last.Data)
		}
		var datum map[string]any
		if err := json.Unmarshal(last.Data, &datum); err != nil {
			t.Fatalf("decode datum: %v; raw=%s", err, last.Data)
		}
		return datum
	}

	d1 := put(`{"upstream_proxy_url": "http://proxy-one.corp.example:3128"}`)
	d2 := put(`{"upstream_proxy_url": "http://proxy-two.corp.example:3128"}`)
	if d1["upstream_proxy_url"] == d2["upstream_proxy_url"] {
		t.Errorf("two PUTs naming different proxy URLs produced the SAME datum value: %v", d1["upstream_proxy_url"])
	}
	if d2["upstream_proxy_url"] != "http://proxy-two.corp.example:3128" {
		t.Errorf("datum upstream_proxy_url = %v, want the URL just written", d2["upstream_proxy_url"])
	}

	d3 := put(`{"upstream_proxy_secret_ref": "corp-proxy-url"}`)
	if d3["upstream_proxy_secret_ref"] != "corp-proxy-url" {
		t.Errorf("datum upstream_proxy_secret_ref = %v, want the ref NAME", d3["upstream_proxy_secret_ref"])
	}

	d4 := put(`{
		"egress_redirects": [{"from": "Registry.NPMJS.org", "to": "artifactory.corp.internal/npm"}],
		"internal_hosts": [{"host_suffix": "svc.cluster.local"}]
	}`)
	redirects, _ := d4["egress_redirects"].([]any)
	if len(redirects) != 1 || redirects[0] != "registry.npmjs.org→artifactory.corp.internal/npm" {
		t.Errorf("datum egress_redirects = %v, want one canonical from→to pair", redirects)
	}
	hosts, _ := d4["internal_hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != "svc.cluster.local" {
		t.Errorf("datum internal_hosts = %v, want [svc.cluster.local]", hosts)
	}
}
