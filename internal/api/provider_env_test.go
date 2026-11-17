// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Under a governing provider block, a run's model credential comes only from
// its provider (owner ruling, 2026-09-25): an env_secret grant that would set a
// model-credential variable is refused at create, Review and dispatch; any
// other env_secret grant is placed as before, and with no block nothing changes.

// joinModelEnvVars is, per arm, three variables the table refuses: the
// harness's own API key variable, one no arm of that harness sets, and one an
// arm of that harness sets that is not in the ruling's list, so only the
// table, never a hard-coded list of credential names, refuses it.
func joinModelEnvVars(agent string) []string {
	if agent == "codex-cli" {
		return []string{"OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_BASE_URL"}
	}
	return []string{"ANTHROPIC_API_KEY", "AWS_ACCESS_KEY_ID", "ANTHROPIC_BASE_URL"}
}

// TestModelEnvNames_TheOwnersNamesAndEveryArmsOwn: the table holds the names
// the ruling lists, and every variable any arm writes — so an arm cannot start
// setting a variable a grant could then set beside it.
func TestModelEnvNames_TheOwnersNamesAndEveryArmsOwn(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID",
		"ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_AUTH_TOKEN", "ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_VERTEX_PROJECT_ID", "ANTHROPIC_AWS_API_KEY", "ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_PROFILE"} {
		if !modelEnvNames[name] {
			t.Errorf("%s is not in modelEnvNames", name)
		}
	}
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
	srv.cfg.AWSSSOProxyInject = true
	srv.cfg.AWSSSOEndpointOverride = "http://sso-fake.test:8090" // the arm also sets the hatch's two variables
	blob := awsSSOBlob{AccessToken: "tok", StartURL: brStartURL, Region: "us-east-1", AccountID: "123456789012",
		RoleName: "BedrockUser", ExpiresAt: time.Now().Add(time.Hour)}
	for _, k := range joinKinds() {
		t.Run(string(k.p.Kind), func(t *testing.T) {
			lane := providerLane{chosen: &chosenProvider{provider: k.p, owner: joinCreateOwner}, blob: blob}
			if providerKeyKind(k.p.Kind) {
				lane.key, _ = providerKeyLaneFor(k.p, k.agent)
			}
			env := map[string]string{}
			srv.applyProviderEnv(context.Background(), types.AgentRun{ID: uuid.New(), Agent: k.agent}, lane, &types.RunPolicySpec{}, env, joinProxyURL)
			if len(env) == 0 {
				t.Fatal("the arm set no env")
			}
			for _, name := range slices.Sorted(maps.Keys(env)) {
				if !modelEnvNames[name] {
					t.Errorf("the %s arm sets %s, which modelEnvNames does not hold", k.p.Kind, name)
				}
			}
		})
	}
}

// TestProviderEnvSecret_DispatchEveryArm: each arm, and a run no provider
// serves, refuses a model-credential env_secret before anything is authored,
// naming the grant and the variable, and places an ordinary one.
func TestProviderEnvSecret_DispatchEveryArm(t *testing.T) {
	happy, noProvider := joinScenarios()[0], joinScenarios()[6]
	for _, k := range joinKinds() {
		for _, sc := range []joinScenario{happy, noProvider} {
			for _, name := range joinModelEnvVars(k.agent) {
				t.Run(fmt.Sprintf("%s/%s/%s refused", k.p.Kind, sc.name, name), func(t *testing.T) {
					plan, ok, st, h, _, env, _, _ := joinDispatch(t, k, sc, envSecretGrant("NPM_TOKEN", "npm-token"),
						envSecretGrant(name, "operator-model-key"))
					if ok || len(plan.injections) != 0 || len(st.grants) != 0 {
						t.Fatalf("dispatch went ahead (ok=%v) with %d grants", ok, len(st.grants))
					}
					if _, set := env[name]; set {
						t.Errorf("%s reached the sandbox env", name)
					}
					msg := fmt.Sprintf(mpRunModelEnvSecret, "operator-model-key", name)
					want := map[string]any{"error": msg, "provider": "", "variable": name, "grant": "operator-model-key"}
					if sc.chosen {
						want["provider"], want["kind"] = k.p.ID, string(k.p.Kind)
					}
					if st.failed != msg {
						t.Errorf("failure hint = %q, want %q", st.failed, msg)
					}
					assertOneRunCreateFailure(t, h, want)
				})
			}
			t.Run(fmt.Sprintf("%s/%s/an ordinary env_secret is placed", k.p.Kind, sc.name), func(t *testing.T) {
				_, ok, st, h, policy, env, _, _ := joinDispatch(t, k, sc, envSecretGrant("NPM_TOKEN", "npm-token"))
				if !ok {
					t.Fatalf("dispatch refused an ordinary env_secret: %q", st.failed)
				}
				h.srv.cfg.Secrets.(*memSecrets).m["npm-token"] = []byte("npm-value-0123456789")
				if placed := h.srv.resolveEnvSecretGrants(context.Background(), st.run, policy, env); !slices.Equal(placed, []string{"NPM_TOKEN"}) ||
					env["NPM_TOKEN"] != "npm-value-0123456789" {
					t.Errorf("placed %v, NPM_TOKEN=%q; want the ordinary grant placed", placed, env["NPM_TOKEN"])
				}
			})
		}
	}
}

// TestProviderEnvSecret_NoBlockRefusedToo: with no provider block there is no
// legacy path left to place the grant (#548) — a model run's credential comes
// only from its model provider, so the same grant is refused the same way.
func TestProviderEnvSecret_NoBlockRefusedToo(t *testing.T) {
	st := &subStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: subOwner}}
	h, sec := newSecretsHarness(t)
	h.srv.cfg.Store = st
	sec.m["operator-aws-key"] = []byte("AKIA-operator-0123456789")
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{envSecretGrant("AWS_ACCESS_KEY_ID", "operator-aws-key")}}
	env := map[string]string{}
	if _, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{}, &policy, env, nil, joinProxyURL,
		artifactRedirectPlan{}, false, types.SiteConfig{}, true, false, bedrockCredUngraded()); ok {
		t.Fatal("dispatch placed a model-credential env_secret with no provider block")
	}
	msg := fmt.Sprintf(mpRunModelEnvSecret, "operator-aws-key", "AWS_ACCESS_KEY_ID")
	if st.failed != msg {
		t.Errorf("failure hint = %q, want %q", st.failed, msg)
	}
	if _, set := env["AWS_ACCESS_KEY_ID"]; set {
		t.Error("AWS_ACCESS_KEY_ID reached the sandbox env")
	}
}

func assertOneRunCreateFailure(t *testing.T, h *harness, want map[string]any) {
	t.Helper()
	var rows []string
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.create" && ev.Outcome == "failure" {
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			got, _ := json.Marshal(d)
			rows = append(rows, string(got))
		}
	}
	if len(rows) != 1 || rows[0] != string(mustJSON(want)) {
		t.Errorf("run.create failure rows = %v\nwant exactly [%s]", rows, mustJSON(want))
	}
}

// TestProviderEnvSecret_DoorsEveryArm: create and Review answer the same 422 —
// provider and kind when a provider was chosen, the sentence naming the grant
// and the variable, and the generic model_provider_unavailable reason (#656
// slice 3), never llmRefusalAuditReason since no sign-in repairs THIS cause —
// and admit an ordinary env_secret; with no block, the same grant is refused
// too.
func TestProviderEnvSecret_DoorsEveryArm(t *testing.T) {
	happy, noProvider := joinScenarios()[0], joinScenarios()[6]
	inline := func(k joinKind, sc joinScenario, grants ...types.GrantSpec) string {
		body := map[string]any{"agent": k.agent, "task": "t", "inline_policy": map[string]any{"min_confinement_class": "CC2", "eligible_grants": grants}}
		if sc.chosen {
			body["model_provider"] = k.p.ID
		}
		b, _ := json.Marshal(body)
		return string(b)
	}
	for _, k := range joinKinds() {
		for _, sc := range []joinScenario{happy, noProvider} {
			name := joinModelEnvVars(k.agent)[0]
			t.Run(fmt.Sprintf("%s/%s", k.p.Kind, sc.name), func(t *testing.T) {
				for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
					w := joinCreate(t, k, sc, path, inline(k, sc, envSecretGrant(name, "operator-model-key")))
					var got errorBody
					_ = json.Unmarshal(w.Body.Bytes(), &got)
					want := errorBody{Error: fmt.Sprintf(mpRunModelEnvSecret, "operator-model-key", name), Reason: string(authz.ReasonModelProviderUnavailable)}
					if sc.chosen {
						want.Provider, want.Kind = k.p.ID, string(k.p.Kind)
					}
					if w.Code != http.StatusUnprocessableEntity || got != want {
						t.Errorf("%s = %d %s\nwant 422 %+v", path, w.Code, w.Body.String(), want)
					}
					w = joinCreate(t, k, sc, path, inline(k, sc, envSecretGrant("NPM_TOKEN", "npm-token")))
					if w.Code != map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK}[path] {
						t.Errorf("%s with an ordinary env_secret = %d %s", path, w.Code, w.Body.String())
					}
					// The same grant on a STORED policy the request names by
					// policy_id: the check reads the resolved spec, however it came.
					t.Run(path+" via policy_id", func(t *testing.T) {
						stored := types.RunPolicy{ID: uuid.New(), Name: "stored", Spec: types.RunPolicySpec{
							MinConfinementClass: types.CC2,
							EligibleGrants:      []types.GrantSpec{envSecretGrant(name, "operator-model-key")},
						}}
						body := map[string]any{"agent": k.agent, "task": "t", "policy_id": stored.ID}
						if sc.chosen {
							body["model_provider"] = k.p.ID
						}
						b, _ := json.Marshal(body)
						w := joinCreate(t, k, sc, path, string(b), stored)
						var got errorBody
						_ = json.Unmarshal(w.Body.Bytes(), &got)
						if w.Code != http.StatusUnprocessableEntity || got != want {
							t.Errorf("%s = %d %s\nwant 422 %+v", path, w.Code, w.Body.String(), want)
						}
					})
				}
			})
		}
	}
	t.Run("no block", func(t *testing.T) {
		for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
			srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
			srv.cfg.Runner = nil
			b, _ := json.Marshal(map[string]any{"agent": "claude-code", "task": "t",
				"inline_policy": map[string]any{"min_confinement_class": "CC2", "eligible_grants": []types.GrantSpec{envSecretGrant("AWS_ACCESS_KEY_ID", "operator-aws-key")}}})
			// An SSO admin session is in the Admin view and cannot launch
			// (refuseAdminViewLaunch); the token lane still can.
			w := do(t, srv, http.MethodPost, path, providerAdminToken(srv, "sub-admit-admin"), string(b))
			var got errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if want := (errorBody{Error: fmt.Sprintf(mpRunModelEnvSecret, "operator-aws-key", "AWS_ACCESS_KEY_ID"), Reason: string(authz.ReasonModelProviderUnavailable)}); w.Code != http.StatusUnprocessableEntity || got != want {
				t.Errorf("%s with no block = %d %s, want 422 %+v", path, w.Code, w.Body.String(), want)
			}
		}
	})
}

// TestClaudeToolSearchDefault: dispatch turns Claude Code's tool search off where claude would reach a host
// that is not first-party Anthropic, as 2.1.231 did, and leaves it at the CLI default everywhere else.
func TestClaudeToolSearchDefault(t *testing.T) {
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
	srv.cfg.AWSSSOProxyInject = true
	srv.cfg.AWSSSOEndpointOverride = "http://sso-fake.test:8090"
	blob := awsSSOBlob{AccessToken: "tok", StartURL: brStartURL, Region: "us-east-1", AccountID: "123456789012",
		RoleName: "BedrockUser", ExpiresAt: time.Now().Add(time.Hour)}
	kinds := map[string]joinKind{}
	for _, k := range joinKinds() {
		kinds[k.p.ID] = k
	}
	laneOf := func(id string) (providerLane, string) {
		k := kinds[id]
		lane := providerLane{chosen: &chosenProvider{provider: k.p, owner: joinCreateOwner}, blob: blob}
		if providerKeyKind(k.p.Kind) {
			lane.key, _ = providerKeyLaneFor(k.p, k.agent)
		}
		return lane, k.agent
	}
	gateway := subProvider("claude-gw")
	gateway.BaseURL = "https://gw.corp.example/anthropic"
	vendorBase := subProvider("claude-vendor")
	vendorBase.BaseURL = "https://api.anthropic.com"
	foundry := providerLane{
		chosen: &chosenProvider{provider: types.ModelProvider{ID: "az", UID: "uid-az", Kind: types.ModelProviderAzureFoundry}, owner: joinCreateOwner},
		azure:  providerAzureLane{harness: azureMessagesHarness, endpoint: "https://res.services.ai.azure.com", model: "m-az"},
	}
	subLane := func(p types.ModelProvider) providerLane {
		return providerLane{chosen: &chosenProvider{provider: p, owner: joinCreateOwner}}
	}
	anthropicKey, _ := laneOf("anthropic-key")
	customEndpoint, _ := laneOf("corp-gw")
	subscription, _ := laneOf("claude")
	sso, _ := laneOf("sso")
	bearer, _ := laneOf("bearer")
	openai, _ := laneOf("openai-key")
	rows := []struct {
		name   string
		lane   providerLane
		run    types.AgentRun
		preset map[string]string // set before the default is applied: an operator's own value
		want   string            // "" = the variable is absent
	}{
		{name: "api key", lane: anthropicKey, run: types.AgentRun{Agent: "claude-code"}, want: "false"},
		{name: "custom endpoint", lane: customEndpoint, run: types.AgentRun{Agent: "claude-code"}, want: "false"},
		{name: "no provider", run: types.AgentRun{Agent: "claude-code"}, want: "false"},
		{name: "subscription behind a gateway", lane: subLane(gateway), run: types.AgentRun{Agent: "claude-code"}, want: "false"},
		{name: "subscription direct", lane: subscription, run: types.AgentRun{Agent: "claude-code"}},
		{name: "subscription set to the vendor host", lane: subLane(vendorBase), run: types.AgentRun{Agent: "claude-code"}},
		{name: "bedrock sso", lane: sso, run: types.AgentRun{Agent: "claude-code"}},
		{name: "bedrock key", lane: bearer, run: types.AgentRun{Agent: "claude-code"}},
		{name: "foundry", lane: foundry, run: types.AgentRun{Agent: "claude-code"}},
		// Foundry and Bedrock stay on the default even when a base URL is also set; only the provider decides.
		{name: "foundry with a base URL", lane: foundry, run: types.AgentRun{Agent: "claude-code"},
			preset: map[string]string{"ANTHROPIC_BASE_URL": "https://gw.corp.example"}},
		{name: "bedrock with a base URL", lane: sso, run: types.AgentRun{Agent: "claude-code"},
			preset: map[string]string{"ANTHROPIC_BASE_URL": "https://gw.corp.example"}},
		{name: "sign-in task", run: types.AgentRun{Agent: "claude-code", Task: harnessLoginTask}},
		{name: "macOS VM, no brokered route", lane: anthropicKey, run: types.AgentRun{Agent: "claude-code", MachineKind: types.MachineMacOSVM}},
		{name: "codex harness", lane: openai, run: types.AgentRun{Agent: "codex-cli"}},
		{name: "operator value kept", lane: anthropicKey, run: types.AgentRun{Agent: "claude-code"},
			preset: map[string]string{"ENABLE_TOOL_SEARCH": "true"}, want: "true"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			env := map[string]string{}
			if r.run.Task != harnessLoginTask {
				srv.applyProviderEnv(context.Background(), r.run, r.lane, &types.RunPolicySpec{}, env, joinProxyURL)
			} else {
				// dispatch's sign-in task: the public host, no model arm.
				env["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
			}
			maps.Copy(env, r.preset)
			applyClaudeToolSearchDefault(r.run, env)
			got, set := env["ENABLE_TOOL_SEARCH"]
			if r.want == "" && set || r.want != "" && got != r.want {
				t.Errorf("ENABLE_TOOL_SEARCH = %q (set %v), want %q (env %v)", got, set, r.want, env)
			}
		})
	}
}
