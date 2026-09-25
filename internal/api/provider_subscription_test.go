// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The per-person Claude subscription (MP-8): a run that chose an
// anthropic_subscription provider is credentialed by its OWNER's own sign-in
// and nobody else's. Every test seeds the OPERATOR's row under the same name
// with a distinguishable token, so "the wrong sign-in was injected" is an
// assertion about which namespace was read.

const (
	subOwner         = "alice@example.com" // mintRunToken's subject
	subOwnerToken    = "sk-ant-oat-alice-own"
	subOperatorToken = "sk-ant-oat-operator-must-not-leak"
	subOtherToken    = "sk-ant-oat-bob-must-not-leak"
)

func subProvider(id string) types.ModelProvider {
	return types.ModelProvider{ID: id, UID: uuid.NewString(), Kind: types.ModelProviderAnthropicSubscription,
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "claude-opus-4-1"}}}
}

func subBlob(tok string) []byte {
	b, _ := json.Marshal(managedCredBlob{Token: tok})
	return b
}

// subStore is the store the dispatch phase and the sink read: grant writes are
// captured, the run and the site config are fixed, and a failed run's hint is
// kept.
type subStore struct {
	store.Store
	run     types.AgentRun
	site    types.SiteConfig
	siteErr error
	grants  []types.CredentialGrant
	failed  string
}

func (s *subStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.grants = append(s.grants, g)
	return g, nil
}
func (s *subStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) { return s.run, nil }
func (s *subStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, s.siteErr
}
func (s *subStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}
func (s *subStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return true, nil
}
func (s *subStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.failed = hint
	return nil
}

// subHarness is a secrets-enabled harness on a subStore whose run is alice's,
// on provider p, with the Claude sign-in image pinned, the shared-subscription
// posture OFF (a multi-user install), and the operator's, alice's and bob's
// rows for p's sentinel all seeded.
func subHarness(t *testing.T, p types.ModelProvider) (*harness, *subStore, *memSecrets) {
	t.Helper()
	h, sec := newSecretsHarness(t)
	st := &subStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: subOwner, ModelProviderID: p.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(p)},
	}
	h.srv.cfg.Store = st
	h.srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	h.srv.cfg.SubscriptionPostureOK = false
	h.srv.cfg.SubscriptionPostureReason = "OIDC/SSO is configured"
	name := providerSecretName(p.UID, providerOAuthPart)
	sec.m[name] = subBlob(subOperatorToken)
	_ = sec.For(subOwner).Put(context.Background(), name, subBlob(subOwnerToken))
	_ = sec.For("bob@example.com").Put(context.Background(), name, subBlob(subOtherToken))
	h.srv.router = h.srv.routes()
	return h, st, sec
}

// dispatchSub runs the real dispatch LLM phase for st.run.
func dispatchSub(h *harness, st *subStore, policy *types.RunPolicySpec, env map[string]string, injections []runner.InjectionGrant) (dispatchLLMPlan, bool) {
	return h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
		policy, env, injections, "", artifactRedirectPlan{}, false, st.site, st.siteErr == nil, false, bedrockCredUngraded())
}

// resolveSub asks the sink to resolve st's first grant as the proxy would.
func resolveSub(t *testing.T, h *harness, st *subStore, host string) (int, string) {
	t.Helper()
	g := st.grants[0]
	var scope struct {
		SecretName string `json:"secret_name"`
	}
	_ = json.Unmarshal(g.Spec.Scope, &scope)
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-sub", Injection: &egress.InjectionRule{
		Host: host, Header: "Authorization", SecretName: scope.SecretName, Format: "Bearer %s",
	}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
	return rr.Code, rr.Body.String()
}

func TestProviderSubscriptionDispatch(t *testing.T) {
	t.Run("the owner's own sign-in, on a multi-user posture, is the only credential authored", func(t *testing.T) {
		p := subProvider("claude")
		h, st, _ := subHarness(t, p)
		policy := &types.RunPolicySpec{
			AllowedDomains:  []string{"example.com"},
			WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.claude", Target: claudeCredTarget}},
		}
		env := map[string]string{}
		stray := uuid.New()
		apiKey := uuid.New()
		plan, ok := dispatchSub(h, st, policy, env, []runner.InjectionGrant{
			{GrantID: stray, Rule: egress.InjectionRule{Host: "evil.example", Header: "Authorization",
				SecretName: providerSecretName(p.UID, providerOAuthPart), Format: "Bearer %s"}},
			{GrantID: apiKey, Rule: egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key",
				SecretName: "anthropic-api-key", Format: "%s"}},
		})
		if !ok || plan.llm.provider == nil || !plan.mitmLLM || plan.mitmCAKeyPEM == "" {
			t.Fatalf("dispatch ok=%v provider=%v mitmLLM=%v, want the provider arm with MITM (failed: %q)",
				ok, plan.llm.provider, plan.mitmLLM, st.failed)
		}
		if len(st.grants) != 1 || len(plan.injections) != 1 {
			t.Fatalf("grants=%d injections=%+v, want exactly the one dispatch authored", len(st.grants), plan.injections)
		}
		ig := plan.injections[0]
		if ig.Rule.SecretName != providerSecretName(p.UID, providerOAuthPart) || ig.Rule.Host != "api.anthropic.com" {
			t.Errorf("injection = %+v, want the provider's sentinel on api.anthropic.com", ig.Rule)
		}
		var scope struct {
			Snapshot providerGrantSnapshot `json:"snapshot"`
		}
		_ = json.Unmarshal(st.grants[0].Spec.Scope, &scope)
		if scope.Snapshot != (providerGrantSnapshot{ProviderUID: p.UID, OwnerSubject: subOwner}) {
			t.Errorf("snapshot = %+v, want {%s %s}", scope.Snapshot, p.UID, subOwner)
		}
		if len(policy.WorkspaceMounts) != 0 {
			t.Errorf("the operator's ~/.claude mount survived: %+v", policy.WorkspaceMounts)
		}
		if env["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" || env["WARDYN_CLAUDE_MANAGED_B64"] == "" ||
			env["ANTHROPIC_MODEL"] != "claude-opus-4-1" || env["ANTHROPIC_API_KEY"] != "" {
			t.Errorf("sandbox env = %v", env)
		}
		if !strings.Contains(strings.Join(auditReasons(t, h.srv, "run.injection.drop"), ","), "model_credential_not_provider_authored") {
			t.Error("the unauthored sign-in injection was not dropped with an audit row")
		}
	})

	t.Run("a route-through gateway is the injected host and a per-run MITM host", func(t *testing.T) {
		p := subProvider("claude")
		p.BaseURL = "https://llm.corp.example:8443/anthropic"
		h, st, _ := subHarness(t, p)
		env := map[string]string{}
		plan, ok := dispatchSub(h, st, &types.RunPolicySpec{}, env, nil)
		if !ok || len(plan.injections) != 1 || plan.injections[0].Rule.Host != "llm.corp.example" {
			t.Fatalf("ok=%v injections=%+v, want one on the gateway host", ok, plan.injections)
		}
		if len(plan.bedrockMITMHosts) != 1 || plan.bedrockMITMHosts[0] != "llm.corp.example:8443" {
			t.Errorf("MITM hosts = %v, want [llm.corp.example:8443]", plan.bedrockMITMHosts)
		}
		if env["ANTHROPIC_BASE_URL"] != p.BaseURL {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", env["ANTHROPIC_BASE_URL"], p.BaseURL)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(*subStore, *memSecrets, types.ModelProvider)
		want  string
	}{
		{"the owner is not signed in, though the operator is", func(st *subStore, sec *memSecrets, p types.ModelProvider) {
			_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, providerOAuthPart))
		}, fmt.Sprintf(mpRunRefusal, "claude", mpSubNotSignedIn, mpRunRemedySignIn)},
		{"the provider was turned off", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			st.site.ModelProviders.Providers[0].Disabled = true
		}, fmt.Sprintf(mpRunRefusal, "claude", mpRunStateOff, mpRunRemedy)},
		{"the provider was deleted", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			st.site.ModelProviders.Providers = nil
		}, fmt.Sprintf(mpRunRefusal, "claude", mpRunStateMissing, mpRunRemedy)},
		{"the provider block is unreadable", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			st.siteErr = fmt.Errorf("store down")
		}, mpRunUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := subProvider("claude")
			h, st, sec := subHarness(t, p)
			tc.setup(st, sec, p)
			if _, ok := dispatchSub(h, st, &types.RunPolicySpec{AllowAllEgress: true}, map[string]string{}, nil); ok {
				t.Fatal("dispatch went ahead")
			}
			if st.failed != tc.want || len(st.grants) != 0 {
				t.Errorf("failed with %q and %d grants\nwant %q and none", st.failed, len(st.grants), tc.want)
			}
		})
	}
}

func TestProviderSubscriptionSink(t *testing.T) {
	authored := func(t *testing.T, p types.ModelProvider) (*harness, *subStore, *memSecrets) {
		t.Helper()
		h, st, sec := subHarness(t, p)
		if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); !ok || len(st.grants) != 1 {
			t.Fatalf("dispatch: ok=%v grants=%d (failed: %q)", ok, len(st.grants), st.failed)
		}
		return h, st, sec
	}

	t.Run("resolves the owner's own sign-in, and the shared-subscription posture does not apply", func(t *testing.T) {
		h, st, _ := authored(t, subProvider("claude"))
		code, body := resolveSub(t, h, st, "api.anthropic.com")
		var resp injectionResponse
		_ = json.Unmarshal([]byte(body), &resp)
		if code != http.StatusOK || resp.Value != "Bearer "+subOwnerToken || resp.Header != "Authorization" {
			t.Fatalf("resolve = %d %s, want alice's own token", code, body)
		}
	})

	t.Run("the legacy shared sentinels keep the posture 403", func(t *testing.T) {
		h, st, _ := authored(t, subProvider("claude"))
		h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j", Injection: sentinelInjection(types.ManagedOAuthSecret)}
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), h.mintRunToken(t, st.run.ID), "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("legacy sentinel off-posture = %d, want 403", rr.Code)
		}
	})

	for _, tc := range []struct {
		name  string
		host  string
		alter func(*subStore, *memSecrets, types.ModelProvider)
		want  int
	}{
		{"the owner removed their sign-in: the operator's never stands in", "api.anthropic.com",
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, providerOAuthPart))
			}, http.StatusFailedDependency},
		{"a grant recording someone else as the owner", "api.anthropic.com",
			func(st *subStore, _ *memSecrets, p types.ModelProvider) {
				st.grants[0].Spec.Scope = subScope(p.UID, "bob@example.com")
			}, http.StatusForbidden},
		{"a grant with no record", "api.anthropic.com",
			func(st *subStore, _ *memSecrets, p types.ModelProvider) {
				st.grants[0].Spec.Scope = json.RawMessage(`{"host":"api.anthropic.com","secret_name":"` +
					providerSecretName(p.UID, providerOAuthPart) + `"}`)
			}, http.StatusForbidden},
		{"a host that is not the provider's", "evil.example", func(*subStore, *memSecrets, types.ModelProvider) {},
			http.StatusForbidden},
		{"the provider was turned off mid-run", "api.anthropic.com",
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].Disabled = true
			}, http.StatusForbidden},
		{"the provider was re-created under a new UID", "api.anthropic.com",
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].UID = uuid.NewString()
			}, http.StatusForbidden},
		{"the run chose another provider", "api.anthropic.com",
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				other := subProvider("other")
				st.site.ModelProviders.Providers = append(st.site.ModelProviders.Providers, other)
				st.run.ModelProviderID = "other"
			}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := subProvider("claude")
			h, st, sec := authored(t, p)
			tc.alter(st, sec, p)
			code, body := resolveSub(t, h, st, tc.host)
			if code != tc.want {
				t.Fatalf("resolve = %d %s, want %d", code, body, tc.want)
			}
			for _, leak := range []string{subOwnerToken, subOperatorToken, subOtherToken} {
				if strings.Contains(body, leak) {
					t.Fatalf("refusal carries a token: %s", body)
				}
			}
			// secret.read's refusal reasons are snake_case (docs/AUDIT-ACTIONS.md, #205).
			ev := lastAuditEvent(t, h.audit.events, "secret.read")
			var d struct{ Reason string }
			_ = json.Unmarshal(ev.Data, &d)
			if ev.Outcome != "failure" || d.Reason == "" || strings.ContainsAny(d.Reason, "- ") {
				t.Errorf("secret.read = %s %s, want a failure with a snake_case reason", ev.Outcome, ev.Data)
			}
		})
	}
}

func subScope(uid, owner string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"host": "api.anthropic.com", "header": "Authorization", "format": "Bearer %s",
		"secret_name": providerSecretName(uid, providerOAuthPart),
		"snapshot":    providerGrantSnapshot{ProviderUID: uid, OwnerSubject: owner},
	})
	return b
}

// TestProviderSubscriptionCreate is create and Review for a chosen Claude
// subscription: the refusal names the provider and why, at both doors, and a
// person who is signed in launches with the choice frozen on the row.
func TestProviderSubscriptionCreate(t *testing.T) {
	const admin = "sub-admit-admin"
	p := subProvider("claude")
	site := types.SiteConfig{ModelProviders: providerBlock(p)}
	for _, tc := range []struct {
		name     string
		image    bool
		signedIn bool
		want     int
		wantBody string
	}{
		{"not signed in", true, false, http.StatusUnprocessableEntity,
			fmt.Sprintf(mpRunRefusal, "claude", mpSubNotSignedIn, mpRunRemedySignIn)},
		{"the sign-in image does not resolve", false, true, http.StatusUnprocessableEntity,
			fmt.Sprintf(mpRunRefusal, "claude", mpSubNoImage, mpRunRemedy)},
		{"signed in", true, true, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
				srv := providerRunFixture(t, site, &capStore{}, nil)
				if tc.image {
					srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
				}
				srv.cfg.Runner = nil
				// The operator's row is seeded under the same name: it must never
				// count as the caller's sign-in.
				name := providerSecretName(p.UID, providerOAuthPart)
				sec := srv.cfg.Secrets.(*memSecrets)
				sec.m[name] = subBlob(subOperatorToken)
				if tc.signedIn {
					_ = sec.For(admin).Put(context.Background(), name, subBlob(subOwnerToken))
				}
				w := doSSO(t, srv, http.MethodPost, path, admitAdminSession(t), `{"agent":"claude-code","task":"t"}`)
				want := tc.want
				if want == http.StatusCreated && path == "/api/v1/runs/preflight" {
					want = http.StatusOK
				}
				var body errorBody
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != want || body.Error != tc.wantBody {
					t.Errorf("%s = %d %s\nwant %d carrying %q", path, w.Code, w.Body.String(), want, tc.wantBody)
				}
				if w.Code == http.StatusCreated {
					var run types.AgentRun
					_ = json.Unmarshal(w.Body.Bytes(), &run)
					if run.ModelProviderID != "claude" {
						t.Errorf("model_provider_id = %q, want claude", run.ModelProviderID)
					}
				}
			}
		})
	}
}

// TestOwnerSubscriptionTokenIsStrict: the per-person managed-token provider
// reads its owner's own row and never the operator's, which Store.For's Get
// falls back to.
func TestOwnerSubscriptionTokenIsStrict(t *testing.T) {
	uid := uuid.NewString()
	name := providerSecretName(uid, providerOAuthPart)
	sec := &memSecrets{m: map[string][]byte{name: subBlob(subOperatorToken)}}
	s := &Server{cfg: Config{Secrets: sec}}
	if tok, err := s.ownerSubscriptionToken(subOwner, uid).Current(context.Background()); err == nil {
		t.Fatalf("an owner with no sign-in read %q", tok.Value)
	}
	_ = sec.For(subOwner).Put(context.Background(), name, subBlob(subOwnerToken))
	if tok, err := s.ownerSubscriptionToken(subOwner, uid).Current(context.Background()); err != nil || tok.Value != subOwnerToken {
		t.Fatalf("owner's own = %q, %v", tok.Value, err)
	}
	if tok, err := s.ownerSubscriptionToken("", uid).Current(context.Background()); err == nil {
		t.Fatalf("no owner read %q", tok.Value)
	}
}
