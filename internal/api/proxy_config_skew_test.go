// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// previousProxyTag is the last release. Operators pin the proxy image apart
// from wardynd (WARDYN_PROXY_IMAGE, k8s.proxyImage), so the config this tree
// writes meets that release's proxy, whose strict decoder refuses any key it
// does not know. Its key set was generated at the tag by
// internal/egress/proxy's configKeyPaths; bump both at each release.
const previousProxyTag = "v0.7.12"

// refusedByPreviousProxy returns the first key in a proxy config that the
// previous release's decoder refuses, or "" if that proxy loads it. It walks
// the JSON the way DisallowUnknownFields does: every object key must be a
// known field, except under a map ("*" in the key set) or an opaque value
// (a key with no children, such as a grant's raw scope).
func refusedByPreviousProxy(t *testing.T, raw []byte) string {
	t.Helper()
	b, err := os.ReadFile("../egress/proxy/testdata/config-keys/" + previousProxyTag + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for k := range strings.FieldsSeq(string(b)) {
		known[k] = true
	}
	under := func(p string) bool {
		return slices.ContainsFunc(slices.Collect(maps.Keys(known)), func(k string) bool { return strings.HasPrefix(k, p) })
	}
	var cfg any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	var walk func(v any, p string) string
	walk = func(v any, p string) string {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				if r := walk(e, p); r != "" {
					return r
				}
			}
		case map[string]any:
			if p != "" && !under(p+".") {
				return "" // opaque: nothing under it is checked
			}
			isMap := p != "" && under(p+".*.")
			for _, k := range slices.Sorted(maps.Keys(x)) {
				kp := strings.TrimPrefix(p+"."+k, ".")
				if isMap {
					kp = p + ".*"
				} else if !known[kp] {
					return kp
				}
				if r := walk(x[k], kp); r != "" {
					return r
				}
			}
		}
		return ""
	}
	return walk(cfg, "")
}

// TestPreviousProxyRefusesWhatItCannotHonour: a run that uses nothing new
// loads on the previous proxy, and a run whose policy uses a feature that
// proxy lacks is refused there naming the key. The refusal is the fail-closed
// half: dispatch hands the policy to the proxy whole, so an older proxy stops
// the run instead of enforcing it without its push rules.
func TestPreviousProxyRefusesWhatItCannotHonour(t *testing.T) {
	base := runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "https://wardynd:8443", ControlPlaneCAPEM: "ca"}
	pushRules := base
	pushRules.Policy.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/"}}
	reviewHold := base
	reviewHold.Policy.PushRules = &types.PushRulesSpec{RequireReviewPaths: []string{"infra/"}, HoldSeconds: 60}
	reviewHold.Unattended = true
	unattended := base
	unattended.Unattended = true
	// The previous proxy knows the grant as ado_grants; its strict decoder
	// refuses ado_grant rather than running with the Azure DevOps gate off.
	adoGrant := base
	adoGrant.ADOGrant = &proxy.ADOGrantConfig{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapRead}, Hosts: []string{"dev.azure.com"}}

	for _, tc := range []struct {
		name string
		pc   runner.ProxyConfig
		want string
	}{
		{"required fields only", base, ""},
		{"push rules", pushRules, "policy.push_rules"},
		{"review hold on an unattended run", reviewHold, "policy.push_rules"},
		// Dispatch sets unattended only beside review paths, which the case
		// above refuses; alone the key is refused too.
		{"unattended", unattended, "unattended"},
		{"azure devops grant", adoGrant, "ado_grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := runner.BuildProxyConfig(uuid.New(), tc.pc, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			if got := refusedByPreviousProxy(t, raw); got != tc.want {
				t.Fatalf("the %s proxy refuses key %q, want %q (\"\" = it loads):\n%s", previousProxyTag, got, tc.want, raw)
			}
		})
	}
}

// TestDispatchConfigLoadsOnPreviousProxy dispatches one run per model provider
// kind through the real POST /runs door and loads the config the runner was
// handed with the previous release's key set. A key this tree adds for every
// run, rather than only for runs that use it, fails every run on an operator's
// pinned previous-release proxy.
func TestDispatchConfigLoadsOnPreviousProxy(t *testing.T) {
	const claude = `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, srv *Server)
		body  string
		// took proves the run dispatched on the lane under test.
		took func(pc runner.ProxyConfig) bool
	}{
		{
			name: "no model credential",
			body: `{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`,
			took: func(pc runner.ProxyConfig) bool { return len(pc.Injection) == 0 },
		},
		{
			name: "anthropic_api_key",
			setup: func(t *testing.T, srv *Server) {
				srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}
			},
			body: apiKeyRun("claude-code", "api.anthropic.com", "x-api-key", "anthropic-api-key"),
			took: injectsHost("api.anthropic.com"),
		},
		{
			name: "openai_api_key",
			setup: func(t *testing.T, srv *Server) {
				srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"openai-api-key": []byte("sk-openai-test")}}
			},
			body: apiKeyRun("codex-cli", "api.openai.com", "Authorization", "openai-api-key"),
			took: injectsHost("api.openai.com"),
		},
		{
			name: "anthropic_subscription",
			setup: func(t *testing.T, srv *Server) {
				srv.cfg.SubscriptionPostureOK = true
				srv.cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}
			},
			body: claude,
			took: func(pc runner.ProxyConfig) bool { return pc.MITMLLM },
		},
		{
			name: "bedrock_bearer",
			setup: func(t *testing.T, srv *Server) {
				c := bedrockBearerCfg()
				srv.cfg.BedrockRegion, srv.cfg.BedrockModel, srv.cfg.Secrets, srv.cfg.MaskRegistry = c.BedrockRegion, c.BedrockModel, c.Secrets, c.MaskRegistry
			},
			body: claude,
			took: func(pc runner.ProxyConfig) bool { return len(pc.Injection) == 1 && len(pc.MITMHosts) == 1 },
		},
		{
			name: "bedrock_sso",
			setup: func(t *testing.T, srv *Server) {
				s := ssoInjectServer(t, true)
				srv.cfg.BedrockRegion, srv.cfg.BedrockModel, srv.cfg.Secrets = s.cfg.BedrockRegion, s.cfg.BedrockModel, s.cfg.Secrets
				srv.cfg.AWSSSOProxyInject, srv.cfg.Now, srv.cfg.MaskRegistry = true, s.cfg.Now, s.cfg.MaskRegistry
			},
			body: claude,
			took: func(pc runner.ProxyConfig) bool { return len(pc.Injection) == 1 && len(pc.MITMHosts) == 1 },
		},
		{
			// Every routing knob the previous release already honoured, on a
			// Bedrock static-key run with a token-bearing artifact redirect.
			name: "bedrock static keys behind every site routing setting",
			setup: func(t *testing.T, srv *Server) {
				c := bedrockStaticCfg()
				srv.cfg.BedrockRegion, srv.cfg.BedrockModel, srv.cfg.MaskRegistry = c.BedrockRegion, c.BedrockModel, c.MaskRegistry
				c.Secrets.(*memSecrets).m["corp-proxy-url"] = []byte("http://proxy.corp:3128")
				c.Secrets.(*memSecrets).m["npm-artifactory-token"] = []byte("s3cr3t-npm-token")
				srv.cfg.Secrets = c.Secrets
				srv.cfg.TrustedCAPEM = "corp-ca"
				srv.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gw.corp.example/anthropic"}
				seedSiteConfig(t, srv, types.SiteConfig{
					UpstreamProxySecretRef: "corp-proxy-url",
					UpstreamProxyNoProxy:   []string{"10.0.0.0/8", ".corp.example"},
					InternalHosts:          []types.InternalHost{{HostSuffix: "artifactory.corp", CIDRs: []string{"10.1.0.0/16"}}},
					EgressRedirects: []types.EgressRedirect{
						{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
					},
				})
			},
			body: `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing","inline_policy":` +
				`{"allowed_domains":["api.anthropic.com","registry.npmjs.org"],"min_confinement_class":"CC2"}}`,
			took: func(pc runner.ProxyConfig) bool {
				return pc.UpstreamProxyURL != "" && len(pc.UpstreamProxyNoProxy) == 2 && len(pc.InternalHosts) == 1 &&
					pc.TrustedCAPEM != "" && len(pc.LLMUpstreams) == 1 && injectsHost("artifactory.corp")(pc)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{}
			srv, _ := pgHarnessWithRunner(t, fr)
			srv.cfg.ControlPlaneURL, srv.cfg.ControlPlaneCAPEM = "https://wardynd:8443", "internal-ca"
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
			}
			fr.waitForSandbox(t)
			spec := fr.lastSpec
			if !tc.took(spec.ProxyConfig) {
				t.Fatalf("the run did not dispatch on the %s lane: ProxyConfig = %+v", tc.name, spec.ProxyConfig)
			}
			raw, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			if k := refusedByPreviousProxy(t, raw); k != "" {
				t.Fatalf("the %s proxy refuses key %q, so every %s run fails at sidecar start on an operator who "+
					"pinned that image. Make the key omitempty and set it only for runs that use its feature.\n%s",
					previousProxyTag, k, tc.name, raw)
			}
		})
	}
}

// apiKeyRun is a run whose policy carries the provider's auto-mint api_key
// grant, the way a workspace or composed policy brings one.
func apiKeyRun(agent, host, header, secret string) string {
	return `{"agent":"` + agent + `","repo":"acme/widgets","task":"do the thing","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["` + host + `"],"eligible_grants":[{"kind":"api_key","scope":` +
		`{"host":"` + host + `","header":"` + header + `","secret_name":"` + secret + `"}}]}}`
}

func injectsHost(host string) func(runner.ProxyConfig) bool {
	return func(pc runner.ProxyConfig) bool {
		return slices.ContainsFunc(pc.Injection, func(g runner.InjectionGrant) bool { return g.Rule.Host == host })
	}
}

// seedSiteConfig stores sc and restores the empty singleton afterwards, so it
// cannot leak into another test sharing WARDYN_TEST_PG.
func seedSiteConfig(t *testing.T, srv *Server, sc types.SiteConfig) {
	t.Helper()
	if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), sc); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })
}
