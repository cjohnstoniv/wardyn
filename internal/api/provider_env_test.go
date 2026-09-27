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
						want["provider"], want["kind"], want["mechanism"] = k.p.ID, string(k.p.Kind), string(k.p.Kind)
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

// TestProviderEnvSecret_NoBlockUnchanged: with no provider block the legacy
// path places the same grant exactly as before.
func TestProviderEnvSecret_NoBlockUnchanged(t *testing.T) {
	st := &subStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: subOwner}}
	h, sec := newSecretsHarness(t)
	h.srv.cfg.Store = st
	sec.m["operator-aws-key"] = []byte("AKIA-operator-0123456789")
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{envSecretGrant("AWS_ACCESS_KEY_ID", "operator-aws-key")}}
	env := map[string]string{}
	if _, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{}, &policy, env, nil, joinProxyURL,
		artifactRedirectPlan{}, false, types.SiteConfig{}, true, false, bedrockCredUngraded()); !ok {
		t.Fatalf("legacy dispatch refused: %q", st.failed)
	}
	if placed := h.srv.resolveEnvSecretGrants(context.Background(), st.run, policy, env); !slices.Equal(placed, []string{"AWS_ACCESS_KEY_ID"}) {
		t.Errorf("placed %v, want AWS_ACCESS_KEY_ID as today", placed)
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
// and the variable, and no reason, since no sign-in repairs it — and admit an
// ordinary env_secret; with no block, the same grant is admitted as before.
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
					want := errorBody{Error: fmt.Sprintf(mpRunModelEnvSecret, "operator-model-key", name)}
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
			w := doSSO(t, srv, http.MethodPost, path, admitAdminSession(t), string(b))
			if w.Code != map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK}[path] {
				t.Errorf("%s with no block = %d %s, want it admitted as before", path, w.Code, w.Body.String())
			}
		}
	})
}
