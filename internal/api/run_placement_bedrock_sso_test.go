// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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

// Create, Review and the policy preview judge the same token cache before
// Launch: with proxy-inject off a local bedrock_sso run is refused
// placement_credential naming it; with proxy-inject on its credentials are
// admitted, so the only refusal left is the unpooled one (no runner pool here).
func TestLocalBedrockSSORefusedBeforeLaunchOnlyWithProxyInjectOff(t *testing.T) {
	for _, inject := range []bool{true, false} {
		srv := createRenewalFixture(t)
		srv.cfg.AWSSSOProxyInject = inject
		blob := createRenewalBlob()
		blob.ExpiresAt = time.Now().Add(time.Hour)
		storeSSOBlobFor(t, srv, createRenewalOwner, blob)
		want := string(placement.ReasonPlacementCredential)
		if inject {
			want = string(placement.ReasonPlacementUnavailable)
		}
		for _, door := range contractDoors {
			body := strings.TrimSuffix(createRenewalBody, "}") + `,"placement":"local"}`
			w := do(t, srv, http.MethodPost, door.path, providerAdminToken(srv, createRenewalOwner), body)
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != want {
				t.Fatalf("inject=%v %s = %d %s, want 422 %s", inject, door.name, w.Code, w.Body.String(), want)
			}
			if !inject && !strings.Contains(w.Body.String(), bedrockSSOSecretEnvField) {
				t.Errorf("%s refusal does not name the token cache: %s", door.name, w.Body.String())
			}
		}
		if runRowCount(srv) != 0 {
			t.Fatalf("inject=%v: a refused local run wrote a row", inject)
		}
	}
}

// The class-killer: for every provider kind and proxy-inject setting, each key
// the dispatch arm (applyProviderEnv) makes splitSecretEnv move onto SecretEnv
// must be covered by a create-time intent (localProviderIntent), or a local run
// passes Review and is refused only after Launch.
func TestLocalProviderSecretEnvKeysHaveCreateTimeIntents(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	checked := 0
	for _, kind := range slices.Sorted(maps.Keys(types.ClosedModelProviderKinds)) {
		for _, inject := range []bool{true, false} {
			f, blob := bedrockSSOLocalFixture(t, inject)
			p := awsSSOTestProvider()
			p.Kind = kind
			run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner, Placement: types.PlacementLocal}
			env := map[string]string{}
			lane := providerLane{chosen: &chosenProvider{provider: p, owner: awsSSOTestOwner}, blob: blob}
			llm := f.srv.applyProviderEnv(context.Background(), run, lane, &types.RunPolicySpec{}, env, "http://proxy.invalid")
			intents, known := f.srv.localProviderIntent(r, awsSSOTestOwner, p)
			if !known {
				t.Fatalf("%s: no create-time intents", kind)
			}
			for key := range splitSecretEnv(env, llm.secretEnvKeys) {
				field := "SandboxSpec.SecretEnv[" + key + "]"
				if !slices.ContainsFunc(intents, func(in placement.CredentialIntent) bool { return in.Field == field }) {
					t.Errorf("%s inject=%v: dispatch places %s with no create-time intent", kind, inject, field)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no provider kind placed a SecretEnv key: the parity check checked nothing")
	}
}
