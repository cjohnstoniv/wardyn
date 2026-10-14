// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// bedrockSSOLocalFixture is a component fixture whose run owner holds a live
// session for awsSSOTestProvider, with proxy-inject set as given.
func bedrockSSOLocalFixture(t *testing.T, inject bool) (*componentFixture, awsSSOBlob) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	f.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	f.srv.cfg.AWSSSOProxyInject = inject
	f.st.siteConfig = awsSSOTestSite()
	return f, putAWSSSOBlob(t, f.srv, time.Now().Add(time.Hour))
}

// A bedrock_sso run placed on the person's own runner, composed the way dispatch
// composes it (the provider arm, splitSecretEnv, the captured-SSO grant through
// createDispatchGrant) and judged by the dispatch-time classification. Under
// proxy-inject the ~/.aws payload holds only a placeholder, so it rides Env and
// the run is admitted with its session delivered own; with proxy-inject off the
// payload is the real token cache, rides SecretEnv and is refused.
func TestLocalBedrockSSODispatchAdmittedOnlyUnderProxyInject(t *testing.T) {
	for _, inject := range []bool{true, false} {
		f, blob := bedrockSSOLocalFixture(t, inject)
		ctx := context.Background()
		run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner, Placement: types.PlacementLocal}
		policy := &types.RunPolicySpec{}
		env := map[string]string{"WARDYN_TASK_MODE": "agent"}
		llm := f.srv.providerBedrockTransport(ctx, run, policy, env, chosenProvider{provider: awsSSOTestProvider(), owner: awsSSOTestOwner}, blob)
		if !llm.bedrockReady || llm.injectBedrockSSO != inject {
			t.Fatalf("inject=%v: transport ready=%v injectSSO=%v", inject, llm.bedrockReady, llm.injectBedrockSSO)
		}
		spec := runner.SandboxSpec{RunID: run.ID, ProxyConfig: runner.ProxyConfig{Policy: *policy}}
		spec.SecretEnv = splitSecretEnv(env, llm.secretEnvKeys)
		spec.Env = env
		if llm.injectBedrockSSO {
			injections, hosts, ok := f.srv.authorBedrockSSOInjection(ctx, run, llm, llm.providerAWSScope(), nil)
			if !ok || len(injections) != 1 {
				t.Fatalf("captured-SSO grant not authored for a local run: ok=%v %+v", ok, injections)
			}
			spec.ProxyConfig.Injection, spec.ProxyConfig.MITMHosts = injections, hosts
			if g := f.st.grants; len(g) != 1 || g[0].Delivery != types.GrantDeliveryOwn || !g[0].Spec.OwnerOnly {
				t.Fatalf("captured-SSO grant is not delivered own: %+v", g)
			}
		}
		p, err := f.srv.localResolvedPlan(ctx, run, false, f.st.siteConfig, spec, nil, llm)
		if err != nil {
			t.Fatal(err)
		}
		_, ref := placement.LocalEligibility(p)
		_, inEnv := spec.Env[awsSSOConfigEnvVar]
		_, inSecretEnv := spec.SecretEnv[awsSSOConfigEnvVar]
		if inject {
			if !inEnv || inSecretEnv {
				t.Errorf("proxy-inject: %s inEnv=%v inSecretEnv=%v, want the placeholder in Env only", awsSSOConfigEnvVar, inEnv, inSecretEnv)
			}
			if ref != nil {
				t.Errorf("proxy-inject: a bedrock_sso run on the person's own runner was refused: %v", ref)
			}
			files := decodeSSOFiles(t, spec.Env[awsSSOConfigEnvVar])
			for _, secret := range []string{blob.AccessToken, blob.RefreshToken, blob.ClientID, blob.ClientSecret} {
				for path, body := range files {
					if strings.Contains(body, secret) {
						t.Errorf("proxy-inject: plain Env %s carries session material %q", path, secret)
					}
				}
			}
			continue
		}
		if inEnv || !inSecretEnv {
			t.Errorf("proxy-inject off: %s inEnv=%v inSecretEnv=%v, want the token cache in SecretEnv only", awsSSOConfigEnvVar, inEnv, inSecretEnv)
		}
		if ref == nil || ref.Reason != placement.ReasonPlacementCredential || ref.Field != "SandboxSpec.SecretEnv["+awsSSOConfigEnvVar+"]" {
			t.Errorf("proxy-inject off: refusal %v, want placement_credential on the token cache", ref)
		}
	}
}

// The class-killer: every key dispatch moves onto SecretEnv for a local run
// (its env_secret grants, the provider arm's keys and the Codex CLI's MCP
// servers: dispatchRun's splitSecretEnv list) has a dispatch-time origin, and
// a key that origin refuses has a create-time intent with the same origin
// (localProviderIntent), so create refuses it before Launch.
func TestLocalSecretEnvKeysHaveCreateTimeIntents(t *testing.T) {
	type scenario struct {
		kind             types.ModelProviderKind
		inject, codexMCP bool
		ownEnvSecret     bool
	}
	var cases []scenario
	for _, kind := range slices.Sorted(maps.Keys(types.ClosedModelProviderKinds)) {
		cases = append(cases, scenario{kind: kind, inject: true}, scenario{kind: kind})
	}
	cases = append(cases, scenario{kind: types.ModelProviderOpenAIAPIKey, inject: true, codexMCP: true},
		scenario{kind: types.ModelProviderAnthropicAPIKey, inject: true, ownEnvSecret: true})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	ctx := context.Background()
	checked := map[string]bool{}
	for _, c := range cases {
		f, blob := bedrockSSOLocalFixture(t, c.inject)
		p := awsSSOTestProvider()
		p.Kind = c.kind
		run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner, Placement: types.PlacementLocal}
		policy := types.RunPolicySpec{}
		if c.codexMCP {
			run.Agent = codexHarness
		}
		if c.ownEnvSecret {
			if err := f.srv.cfg.Secrets.For(awsSSOTestOwner).Put(ctx, "my-token", []byte("own-token-value")); err != nil {
				t.Fatal(err)
			}
			g := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantEnvSecret, OwnerOnly: true,
				Scope: json.RawMessage(`{"name":"MY_TOKEN","secret_name":"my-token"}`)}}
			f.st.grants = append(f.st.grants, g)
			policy.EligibleGrants = []types.GrantSpec{g.Spec}
		}
		env := map[string]string{}
		lane := providerLane{chosen: &chosenProvider{provider: p, owner: awsSSOTestOwner}, blob: blob}
		llm := f.srv.applyProviderEnv(ctx, run, lane, &policy, env, "http://proxy.invalid")
		envKeys := f.srv.resolveEnvSecretGrants(ctx, run, policy, env)
		if c.codexMCP {
			var injections []runner.InjectionGrant
			dispatchMCP([]componentMCP{{Key: "docs", URL: "https://mcp.example/", Host: "mcp.example", Harnesses: []string{codexHarness}}}, &injections, "http://proxy.invalid", false, env)
		}
		spec := runner.SandboxSpec{RunID: run.ID, ProxyConfig: runner.ProxyConfig{Policy: policy}}
		spec.SecretEnv = splitSecretEnv(env, slices.Concat(envKeys, llm.secretEnvKeys, []string{codexMCPEnv}))
		spec.Env = env
		plan, err := f.srv.localResolvedPlan(ctx, run, false, f.st.siteConfig, spec, nil, llm)
		if err != nil {
			t.Fatal(err)
		}
		intents, known := f.srv.localProviderIntent(r, awsSSOTestOwner, p)
		if !known {
			t.Fatalf("%s: no create-time intents", c.kind)
		}
		for key, value := range spec.SecretEnv {
			field := "SandboxSpec.SecretEnv[" + key + "]"
			origin, found := plan.Origins[field]
			if !found {
				t.Errorf("%+v: dispatch places %s with no origin: refused after Launch", c, field)
				continue
			}
			checked[key] = true
			at := slices.IndexFunc(intents, func(in placement.CredentialIntent) bool { return in.Field == field })
			if at >= 0 {
				if intents[at].Origin != origin {
					t.Errorf("%+v: %s create-time origin %+v, dispatch stamps %+v", c, field, intents[at].Origin, origin)
				}
				continue
			}
			alone := placement.LocalPlan{Spec: runner.SandboxSpec{SecretEnv: map[string]string{key: value}}, Origins: map[string]placement.CredentialOrigin{field: origin}, Delivery: plan.Delivery}
			if _, ref := placement.LocalEligibility(alone); ref != nil {
				t.Errorf("%+v: dispatch refuses %s after Launch with no create-time intent: %v", c, field, ref)
			}
		}
	}
	for _, key := range []string{awsSSOConfigEnvVar, codexMCPEnv, "MY_TOKEN"} {
		if !checked[key] {
			t.Errorf("no scenario placed %s on SecretEnv: the parity check did not check it", key)
		}
	}
}
