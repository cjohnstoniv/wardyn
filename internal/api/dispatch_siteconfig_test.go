// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file validates the SINGLE composition site (dispatchWithVerify in
// runs.go) that a recent 4-lane merge assembled by hand: operator upstream
// proxy (site-config), artifact-registry redirect (site-config), SCM host
// union (site-config), and Bedrock auth (Server Config + secrets) must all
// land on the SAME runner.SandboxSpec for one real dispatch. Each lane already
// has its own unit tests (site_config_test.go, bedrock_test.go,
// artifact_redirect_test.go, upstream_proxy_test.go) — this file proves the
// WHOLE composition, especially that ProxyConfig.UpstreamProxyURL and
// ProxyConfig.MITMHosts (authored by two different lanes) both end up set on
// the one ProxyConfig the merge conflict had to reconcile by hand.
//
// Reuses interactive_test.go's fakeRunner (captures lastSpec on CreateSandbox)
// and pgHarnessWithRunner (Postgres-backed Server, skips cleanly without
// WARDYN_TEST_PG) rather than reinventing either.

// TestDispatch_SiteConfigComposition_ProxyArtifactScmBedrock drives a real
// POST /api/v1/runs -> handleCreateRun -> dispatch -> dispatchWithVerify and
// asserts the composed runner.SandboxSpec the fakeRunner captured.
func TestDispatch_SiteConfigComposition_ProxyArtifactScmBedrock(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	// Bedrock: boot-time Config (region/model) + resident AWS secrets — the
	// resolveBedrockAuth readiness gate. No subscription mount on this run, so
	// Bedrock (not api-key) must win.
	srv.cfg.BedrockRegion = "us-east-1"
	srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{
		"corp-proxy-url":             []byte("http://proxy.corp:3128"),
		"npm-artifactory-token":      []byte("s3cr3t-npm-token"),
		bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
		bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
	}}

	// Operator SiteConfig: upstream corp proxy, npm egress redirect (with a
	// token so the MITM+injection half is exercised too), and a declared GHES
	// SCM host. site_config is a store-wide (not per-run) singleton row, so
	// restore the zero value afterward — otherwise a value seeded here leaks
	// into any other PG-backed test sharing WARDYN_TEST_PG.
	ctx := context.Background()
	if _, err := srv.cfg.Store.PutSiteConfig(ctx, types.SiteConfig{
		UpstreamProxySecretRef: "corp-proxy-url",
		EgressRedirects: []types.EgressRedirect{
			{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
		},
		ScmHosts: []string{"ghes.corp.example"},
	}); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() {
		_, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{})
	})

	spec := dispatchAndCaptureSpec(t, srv, fr)
	assertProxyArtifactScmBedrockComposition(t, spec)
}

// TestDispatch_SiteConfigComposition_LegacyArtifactOverridesFold is the
// dispatch-level half of the fold-compat golden the Wave A gate requires: the
// SAME composition as TestDispatch_SiteConfigComposition_ProxyArtifactScmBedrock,
// seeded via a PUT /site-config body in the DEPRECATED artifact_overrides shape
// instead of EgressRedirects directly — proving the request-decode fold
// (foldLegacyArtifactOverrides, site_config.go) produces a BYTE-IDENTICAL
// dispatched SandboxSpec to the current canonical shape, not just an
// isolated-unit match.
func TestDispatch_SiteConfigComposition_LegacyArtifactOverridesFold(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	srv.cfg.BedrockRegion = "us-east-1"
	srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{
		"corp-proxy-url":             []byte("http://proxy.corp:3128"),
		"npm-artifactory-token":      []byte("s3cr3t-npm-token"),
		bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
		bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
	}}

	legacyBody := `{
		"upstream_proxy_secret_ref": "corp-proxy-url",
		"artifact_overrides": {"npm": {"base_url": "https://artifactory.corp/npm", "token_secret_ref": "npm-artifactory-token"}},
		"scm_hosts": ["ghes.corp.example"]
	}`
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, legacyBody)
	if w.Code != http.StatusOK {
		t.Fatalf("seed legacy site config: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		_, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{})
	})

	spec := dispatchAndCaptureSpec(t, srv, fr)
	assertProxyArtifactScmBedrockComposition(t, spec)
}

// dispatchAndCaptureSpec drives POST /api/v1/runs with the shared inline policy
// both site-config composition tests need (registry.npmjs.org present in the
// STARTING egress allowlist, so the substitution assertion below is real —
// proves removal — rather than vacuous) and returns the fakeRunner-captured
// SandboxSpec.
func dispatchAndCaptureSpec(t *testing.T, srv *Server, fr *fakeRunner) runner.SandboxSpec {
	t.Helper()
	const dispatchInlinePolicy = `{"allowed_domains":["api.anthropic.com","registry.npmjs.org"],"min_confinement_class":"CC2"}`
	body := `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing","inline_policy":` + dispatchInlinePolicy + `}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	return fr.lastSpec
}

// assertProxyArtifactScmBedrockComposition is the shared assertion body for
// both the canonical-shape and legacy-fold composition tests: same platform
// env, same Bedrock wiring, same upstream-proxy + artifact-MITM ProxyConfig,
// same token injection (never resident in Env), same AllowedDomains
// substitution. Kept as ONE function so the two tests can never silently drift
// apart on what "the same dispatch behavior" means.
func assertProxyArtifactScmBedrockComposition(t *testing.T, spec runner.SandboxSpec) {
	t.Helper()

	// 1. Platform sandboxEnv + artifact config delivery + the Bedrock switch —
	// all riding the same Env map.
	for _, k := range []string{"GOTMPDIR", "GOCACHE", "MAVEN_OPTS"} {
		if spec.Env[k] == "" {
			t.Errorf("Env[%q] empty, want platform toolchain env set", k)
		}
	}
	if spec.Env["WARDYN_ARTIFACT_CONFIG_B64"] == "" {
		t.Error("Env[WARDYN_ARTIFACT_CONFIG_B64] empty, want the npm redirect config materialized")
	}
	if spec.Env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Errorf("Env[CLAUDE_CODE_USE_BEDROCK] = %q, want \"1\" (no subscription mount + Bedrock ready => Bedrock wins)", spec.Env["CLAUDE_CODE_USE_BEDROCK"])
	}
	if spec.Env["AWS_REGION"] != "us-east-1" {
		t.Errorf("Env[AWS_REGION] = %q, want us-east-1", spec.Env["AWS_REGION"])
	}
	if spec.Env["AWS_ACCESS_KEY_ID"] != "AKIATESTTESTTESTTEST" {
		t.Errorf("Env[AWS_ACCESS_KEY_ID] = %q, want the resident test key", spec.Env["AWS_ACCESS_KEY_ID"])
	}

	// 2 + 3 TOGETHER — the specific merge-conflict-resolution assertion: the
	// upstream corp proxy (wiring lane) and the artifact-redirect MITM host
	// (artifact lane) both land on the SAME ProxyConfig.
	if spec.ProxyConfig.UpstreamProxyURL != "http://proxy.corp:3128" {
		t.Errorf("ProxyConfig.UpstreamProxyURL = %q, want http://proxy.corp:3128", spec.ProxyConfig.UpstreamProxyURL)
	}
	// PORT-QUALIFIED, not bare: 4d8f48e1 (W13-S1-5) made planArtifactRedirect
	// author net.JoinHostPort(host, redirectPort(r.To)), so the proxy's MITM dial
	// lands on the port the operator configured instead of assuming 443. The
	// redirect above has no explicit port, so 443 is the derived one. The proxy
	// splits the suffix back off (parseMITMHostPort, proxy.go) and scopes the
	// entry to that port, so this is the correct wire shape — this expectation
	// predates the change (test last touched d3c1f103) and was stale, not the
	// code. TestRedirectPort (artifact_redirect_test.go) is the producer-side half.
	const wantMITM = "artifactory.corp:443"
	foundMITM := false
	for _, h := range spec.ProxyConfig.MITMHosts {
		if h == wantMITM {
			foundMITM = true
		} else {
			t.Errorf("ProxyConfig.MITMHosts contains unexpected host %q (want ONLY the configured artifact host)", h)
		}
	}
	if !foundMITM {
		t.Errorf("ProxyConfig.MITMHosts = %v, want %s present", spec.ProxyConfig.MITMHosts, wantMITM)
	}

	// 4. Token injection: an Authorization/Bearer rule for artifactory.corp,
	// naming the secret by reference — never resident in Env.
	foundInjection := false
	for _, ig := range spec.ProxyConfig.Injection {
		if ig.Rule.Host == "artifactory.corp" {
			foundInjection = true
			if ig.Rule.Header != "Authorization" || ig.Rule.Format != "Bearer %s" {
				t.Errorf("artifactory.corp injection rule = %+v, want Authorization/Bearer %%s", ig.Rule)
			}
			if ig.Rule.SecretName != "npm-artifactory-token" {
				t.Errorf("artifactory.corp injection SecretName = %q, want npm-artifactory-token", ig.Rule.SecretName)
			}
		}
	}
	if !foundInjection {
		t.Errorf("no Injection rule for artifactory.corp; got %+v", spec.ProxyConfig.Injection)
	}
	for k, v := range spec.Env {
		if strings.Contains(v, "s3cr3t-npm-token") {
			t.Errorf("Env[%q] leaks the artifact token; it must only ride the proxy-side injection, never sandbox env", k)
		}
	}

	// 5. AllowedDomains: npm public host substituted OUT for the corp mirror,
	// the declared GHES scm host and both Bedrock hosts unioned in.
	domains := spec.ProxyConfig.Policy.AllowedDomains
	has := func(h string) bool {
		for _, d := range domains {
			if d == h {
				return true
			}
		}
		return false
	}
	if has("registry.npmjs.org") {
		t.Errorf("AllowedDomains still contains registry.npmjs.org, want substituted out; got %v", domains)
	}
	for _, want := range []string{
		"artifactory.corp",
		"ghes.corp.example",
		"bedrock-runtime.us-east-1.amazonaws.com",
		"bedrock.us-east-1.amazonaws.com",
	} {
		if !has(want) {
			t.Errorf("AllowedDomains missing %q; got %v", want, domains)
		}
	}
}

// TestDispatch_BedrockAbsentCreds_FallsBackToAPIKeyPlaceholder is the
// precedence negative case: Bedrock region+model are configured but the
// resident AWS credential secrets are ABSENT (a real, non-fatal
// misconfiguration per resolveBedrockAuth's doc comment). Dispatch must NOT
// half-wire Bedrock — no CLAUDE_CODE_USE_BEDROCK — and must fall back to the
// existing proxy-injected api-key placeholder.
func TestDispatch_BedrockAbsentCreds_FallsBackToAPIKeyPlaceholder(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.BedrockRegion = "us-east-1"
	srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}} // no aws-* secrets stored

	body := `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	spec := fr.lastSpec
	if _, ok := spec.Env["CLAUDE_CODE_USE_BEDROCK"]; ok {
		t.Errorf("Env[CLAUDE_CODE_USE_BEDROCK] present with no AWS creds stored; want absent (fallback, not a half-wired Bedrock)")
	}
	if spec.Env["ANTHROPIC_API_KEY"] != "wardyn-proxy-injected" {
		t.Errorf("Env[ANTHROPIC_API_KEY] = %q, want the proxy-injected sentinel (api-key fallback)", spec.Env["ANTHROPIC_API_KEY"])
	}
}
