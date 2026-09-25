// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Bedrock provider arms (MP-9): a run that chose a bedrock_sso or
// bedrock_bearer provider reaches Bedrock where the provider record says, on
// its OWNER's own credential. Every test also seeds what the legacy chain
// would have served — the operator's bedrock-api-key, the operator's captured
// session, static SigV4 keys, a host ~/.aws dir and the boot region/model —
// plus the operator's and another person's rows under the provider's own
// names, so "the wrong credential was used" is an assertion about which
// namespace and which lane was read.

const (
	brOwnerKey    = "bedrock-key-alice-own-0123456789"
	brOperatorKey = "bedrock-key-operator-must-not-leak"
	brOtherKey    = "bedrock-key-bob-must-not-leak-0000"
	brOwnerToken  = "sso-access-alice-own-0123456789"
	brOperatorTok = "sso-access-operator-must-not-leak"
	brStartURL    = "https://acme.awsapps.com/start"
	brModel       = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	brBaseURL     = "https://vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com"
)

func brBearerProvider() types.ModelProvider {
	return types.ModelProvider{ID: "bedrock-key", UID: uuid.NewString(), Kind: types.ModelProviderBedrockBearer,
		Bedrock:   &types.BedrockSettings{Region: "us-west-2", BaseURL: brBaseURL},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: brModel}}}
}

func brSSOProvider() types.ModelProvider {
	return types.ModelProvider{ID: "bedrock-prod", UID: uuid.NewString(), Kind: types.ModelProviderBedrockSSO,
		Bedrock: &types.BedrockSettings{Region: "us-west-2", SSOStartURL: brStartURL,
			SSOAccountID: "123456789012", SSORoleName: "BedrockUser"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: brModel}}}
}

func brBlob(tok, account, role string, expires time.Time) []byte {
	b, _ := json.Marshal(awsSSOBlob{AccessToken: tok, StartURL: brStartURL, Region: "us-east-1",
		AccountID: account, RoleName: role, ExpiresAt: expires, CapturedAt: time.Now()})
	return b
}

// brHarness is subHarness for a Bedrock provider p, with every credential the
// legacy chain could reach seeded, and alice's own credential for p stored.
func brHarness(t *testing.T, p types.ModelProvider) (*harness, *subStore, *memSecrets) {
	t.Helper()
	h, st, sec := subHarness(t, p)
	h.srv.cfg.BedrockRegion, h.srv.cfg.BedrockModel = "eu-central-1", "boot-model-must-not-win"
	h.srv.cfg.BedrockBaseURL = "https://boot.example"
	h.srv.cfg.BedrockAWSConfigDir = t.TempDir()
	h.srv.cfg.AWSSSOProxyInject = true
	ctx := context.Background()
	later := time.Now().Add(8 * time.Hour)
	for name, v := range map[string][]byte{
		bedrockAPIKeySecret: []byte(brOperatorKey), bedrockAccessKeyIDSecret: []byte("AKIAOPERATOR"),
		bedrockSecretAccessKeySecret:               []byte("operator-secret-access-key"),
		harnessCredSecretName(awsSSOProvider):      brBlob(brOperatorTok, "999999999999", "Admin", later),
		providerSecretName(p.UID, providerKeyPart): []byte(brOperatorKey),
		providerSecretName(p.UID, providerSSOPart): brBlob(brOperatorTok, "123456789012", "BedrockUser", later),
	} {
		sec.m[name] = v
	}
	_ = sec.For("bob@example.com").Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte(brOtherKey))
	if p.Kind == types.ModelProviderBedrockBearer {
		_ = sec.For(subOwner).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte(brOwnerKey))
	} else {
		_ = sec.For(subOwner).Put(ctx, providerSecretName(p.UID, providerSSOPart), brBlob(brOwnerToken, "123456789012", "BedrockUser", later))
	}
	return h, st, sec
}

func TestProviderBedrockDispatch(t *testing.T) {
	t.Run("bearer: the owner's own key, where the provider says, is the only credential authored", func(t *testing.T) {
		p := brBearerProvider()
		h, st, _ := brHarness(t, p)
		policy := &types.RunPolicySpec{
			AllowedDomains:  []string{"example.com"},
			WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.claude", Target: claudeCredTarget}},
		}
		env := map[string]string{}
		plan, ok := dispatchSub(h, st, policy, env, []runner.InjectionGrant{
			{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "evil.example", Header: "Authorization",
				SecretName: providerSecretName(p.UID, providerKeyPart), Format: "Bearer %s"}},
			{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key",
				SecretName: "anthropic-api-key", Format: "%s"}},
			{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "api.anthropic.com", Header: "Authorization",
				SecretName: types.ManagedOAuthSecret, Format: "Bearer %s"}},
		})
		if !ok || !plan.llm.injectBedrockBearer || plan.llm.bedrock.awsMount || plan.mitmLLM {
			t.Fatalf("dispatch ok=%v bearer=%v mount=%v mitmLLM=%v (failed: %q)",
				ok, plan.llm.injectBedrockBearer, plan.llm.bedrock.awsMount, plan.mitmLLM, st.failed)
		}
		if len(st.grants) != 1 || len(plan.injections) != 1 {
			t.Fatalf("grants=%d injections=%+v, want exactly the one dispatch authored", len(st.grants), plan.injections)
		}
		const host = "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com"
		if r := plan.injections[0].Rule; r.SecretName != providerSecretName(p.UID, providerKeyPart) || r.Host != host {
			t.Errorf("injection = %+v, want the provider's key on %s", r, host)
		}
		var scope struct {
			Snapshot providerGrantSnapshot `json:"snapshot"`
		}
		_ = json.Unmarshal(st.grants[0].Spec.Scope, &scope)
		if scope.Snapshot != (providerGrantSnapshot{ProviderUID: p.UID, OwnerSubject: subOwner}) {
			t.Errorf("snapshot = %+v", scope.Snapshot)
		}
		if !slices.Equal(plan.bedrockMITMHosts, []string{host + ":443"}) {
			t.Errorf("MITM hosts = %v", plan.bedrockMITMHosts)
		}
		want := map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-west-2", "ANTHROPIC_MODEL": brModel,
			"ANTHROPIC_BEDROCK_BASE_URL": brBaseURL, "AWS_BEARER_TOKEN_BEDROCK": "wardyn-proxy-injected",
		}
		for k, v := range want {
			if env[k] != v {
				t.Errorf("env[%s] = %q, want %q", k, env[k], v)
			}
		}
		if env["AWS_ACCESS_KEY_ID"] != "" || len(plan.llm.secretEnvKeys) != 0 {
			t.Errorf("resident SigV4 reached the sandbox: %v", env)
		}
		for _, d := range []string{host, "bedrock.us-west-2.amazonaws.com"} {
			if !slices.Contains(policy.AllowedDomains, d) {
				t.Errorf("egress %v lacks %s", policy.AllowedDomains, d)
			}
		}
		if len(policy.WorkspaceMounts) != 0 {
			t.Errorf("the operator's ~/.claude mount survived: %+v", policy.WorkspaceMounts)
		}
		reasons := strings.Join(auditReasons(t, h.srv, "run.injection.dropped"), ",")
		for _, r := range []string{"model_credential_not_provider_authored"} {
			if !strings.Contains(reasons, r) {
				t.Errorf("dropped reasons %q lack %s", reasons, r)
			}
		}
	})

	// #504 on a provider run: the create door grades a Bedrock provider's run
	// WITH its credential (runProviderChoice.modelCredential), so dispatch's
	// bedrockCredGradeHolds lets it through; graded without one, it would not.
	t.Run("bearer: graded as the create door grades it, the run dispatches", func(t *testing.T) {
		p := brBearerProvider()
		h, st, _ := brHarness(t, p)
		grade := bedrockCredGradedAs(runProviderChoice{provider: p, chosen: true}.modelCredential())
		if grade.host != providerBedrockRuntimeHost(p) {
			t.Fatalf("graded host = %q, want the provider's %q", grade.host, providerBedrockRuntimeHost(p))
		}
		_, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
			&types.RunPolicySpec{}, map[string]string{}, nil, "", artifactRedirectPlan{}, false, st.site, true, false, grade)
		if !ok {
			t.Fatalf("a Bedrock provider run graded with its credential was refused: %q", st.failed)
		}
		h, st, _ = brHarness(t, p)
		if _, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
			&types.RunPolicySpec{}, map[string]string{}, nil, "", artifactRedirectPlan{}, false, st.site, true, false,
			bedrockCredGrade{graded: true}); ok {
			t.Fatal("a Bedrock provider run graded WITHOUT its credential dispatched")
		}
	})

	t.Run("sso: the owner's own session, never the roster's", func(t *testing.T) {
		p := brSSOProvider()
		h, st, _ := brHarness(t, p)
		env := map[string]string{}
		plan, ok := dispatchSub(h, st, &types.RunPolicySpec{}, env, nil)
		if !ok || !plan.llm.injectBedrockSSO || plan.llm.bedrock.awsMount {
			t.Fatalf("dispatch ok=%v sso=%v mount=%v (failed: %q)", ok, plan.llm.injectBedrockSSO, plan.llm.bedrock.awsMount, st.failed)
		}
		if len(st.grants) != 1 || plan.injections[0].Rule.SecretName != types.AWSSSOAccessTokenSecret {
			t.Fatalf("grants=%d injections=%+v, want the one session grant", len(st.grants), plan.injections)
		}
		var scope struct {
			Snapshot awsSSOScopeSnapshot `json:"snapshot"`
		}
		_ = json.Unmarshal(st.grants[0].Spec.Scope, &scope)
		if scope.Snapshot.ProviderUID != p.UID || scope.Snapshot.OwnerSubject != subOwner {
			t.Errorf("snapshot = %+v", scope.Snapshot)
		}
		if env["AWS_REGION"] != "us-west-2" || env["ANTHROPIC_MODEL"] != brModel || env["ANTHROPIC_BEDROCK_BASE_URL"] != "" {
			t.Errorf("env = %v", env)
		}
		if !slices.Contains(plan.llm.secretEnvKeys, awsSSOConfigEnvVar) {
			t.Errorf("the session config is not on SecretEnv: %v", plan.llm.secretEnvKeys)
		}
	})

	later := time.Now().Add(8 * time.Hour)
	for _, tc := range []struct {
		name  string
		p     types.ModelProvider
		setup func(*subStore, *memSecrets, types.ModelProvider)
		want  string
	}{
		{"bearer: the owner has no key, though the operator does", brBearerProvider(),
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, providerKeyPart))
			}, fmt.Sprintf(mpRunRefusal, "bedrock-key", mpRunNoKey, mpRunRemedySignIn)},
		{"bearer: the owner's key is blank", brBearerProvider(),
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("  "))
			}, fmt.Sprintf(mpRunRefusal, "bedrock-key", mpRunNoKey, mpRunRemedySignIn)},
		{"sso: the owner is not signed in, though the operator is", brSSOProvider(),
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, providerSSOPart))
			}, fmt.Sprintf(mpRunRefusal, "bedrock-prod", mpBRNotSignedIn, mpRunRemedySignIn)},
		{"sso: the session is for an account the provider does not pin", brSSOProvider(),
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
					brBlob(brOwnerToken, "222222222222", "BedrockUser", later))
			}, fmt.Sprintf(mpRunRefusal, "bedrock-prod",
				fmt.Sprintf(mpBRPinned, "222222222222", "BedrockUser", "123456789012", "BedrockUser"), mpRunRemedySignIn)},
		{"sso: the session is for another access portal", brSSOProvider(),
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].Bedrock.SSOStartURL = "https://other.awsapps.com/start"
			}, fmt.Sprintf(mpRunRefusal, "bedrock-prod", mpBRPortal, mpRunRemedySignIn)},
		{"sso: the session lapsed and cannot be renewed", brSSOProvider(),
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
					brBlob(brOwnerToken, "123456789012", "BedrockUser", time.Now().Add(-time.Hour)))
			}, fmt.Sprintf(mpRunRefusal, "bedrock-prod", mpBRNotSignedIn, mpRunRemedySignIn)},
		{"the provider was turned off", brBearerProvider(),
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].Disabled = true
			}, fmt.Sprintf(mpRunRefusal, "bedrock-key", mpRunStateOff, mpRunRemedy)},
		{"the provider block is unreadable", brSSOProvider(),
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) { st.siteErr = fmt.Errorf("store down") },
			mpRunUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, sec := brHarness(t, tc.p)
			tc.setup(st, sec, tc.p)
			if _, ok := dispatchSub(h, st, &types.RunPolicySpec{AllowAllEgress: true}, map[string]string{}, nil); ok {
				t.Fatal("dispatch went ahead")
			}
			if st.failed != tc.want || len(st.grants) != 0 {
				t.Errorf("failed with %q and %d grants\nwant %q and none", st.failed, len(st.grants), tc.want)
			}
		})
	}
}

// resolveBR asks the sink to resolve st's first grant, as authored, on host.
func resolveBR(t *testing.T, h *harness, st *subStore, host string) (int, string) {
	t.Helper()
	g := st.grants[0]
	var scope struct {
		SecretName string `json:"secret_name"`
		Header     string `json:"header"`
		Format     string `json:"format"`
	}
	_ = json.Unmarshal(g.Spec.Scope, &scope)
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-br", Injection: &egress.InjectionRule{
		Host: host, Header: scope.Header, SecretName: scope.SecretName, Format: scope.Format,
	}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
	return rr.Code, rr.Body.String()
}

func brAuthored(t *testing.T, p types.ModelProvider) (*harness, *subStore, *memSecrets) {
	t.Helper()
	h, st, sec := brHarness(t, p)
	if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); !ok || len(st.grants) != 1 {
		t.Fatalf("dispatch: ok=%v grants=%d (failed: %q)", ok, len(st.grants), st.failed)
	}
	return h, st, sec
}

// #518 records run.llm.bedrock past dispatch's last gate, from the row
// applyBedrockTransport computes; the provider arm has to hand its row over the
// same way, and the row still names the provider the run chose (#530).
func TestProviderBedrockDispatchAuditsItsTransport(t *testing.T) {
	for _, tc := range []struct {
		p    types.ModelProvider
		mode string
	}{{brBearerProvider(), "bearer"}, {brSSOProvider(), "sso-inject-proxy"}} {
		t.Run(string(tc.p.Kind), func(t *testing.T) {
			h, st, _ := brHarness(t, tc.p)
			if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); !ok {
				t.Fatalf("dispatch refused: %q", st.failed)
			}
			var rows []map[string]any
			for _, ev := range h.srv.cfg.Audit.(*recRecorder).snapshot() {
				if ev.Action != "run.llm.bedrock" {
					continue
				}
				var d map[string]any
				if err := json.Unmarshal(ev.Data, &d); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, d)
			}
			if len(rows) != 1 {
				t.Fatalf("run.llm.bedrock rows = %d, want 1", len(rows))
			}
			d := rows[0]
			if d["provider"] != tc.p.ID || d["region"] != "us-west-2" || d["model"] != brModel ||
				d["endpoint"] != providerBedrockRuntimeHost(tc.p) || d["mode"] != tc.mode {
				t.Errorf("run.llm.bedrock = %v, want provider %s, us-west-2, %s at %s, mode %s",
					d, tc.p.ID, brModel, providerBedrockRuntimeHost(tc.p), tc.mode)
			}
		})
	}
}

func noBRLeak(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{brOwnerKey, brOperatorKey, brOtherKey, brOwnerToken, brOperatorTok} {
		if strings.Contains(body, leak) {
			t.Fatalf("refusal carries a credential: %s", body)
		}
	}
}

func TestProviderBedrockKeySink(t *testing.T) {
	const host = "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com"
	t.Run("resolves the owner's own key", func(t *testing.T) {
		h, st, _ := brAuthored(t, brBearerProvider())
		code, body := resolveBR(t, h, st, host)
		var resp injectionResponse
		_ = json.Unmarshal([]byte(body), &resp)
		if code != http.StatusOK || resp.Value != "Bearer "+brOwnerKey || resp.Header != "Authorization" {
			t.Fatalf("resolve = %d %s, want alice's own key", code, body)
		}
	})

	for _, tc := range []struct {
		name  string
		host  string
		alter func(*subStore, *memSecrets, types.ModelProvider)
		want  int
	}{
		{"the owner removed their key: the operator's never stands in", host,
			func(_ *subStore, sec *memSecrets, p types.ModelProvider) {
				_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, providerKeyPart))
			}, http.StatusFailedDependency},
		{"a grant recording someone else as the owner", host,
			func(st *subStore, _ *memSecrets, p types.ModelProvider) {
				st.grants[0].Spec.Scope = brKeyScope(host, p.UID, "bob@example.com")
			}, http.StatusForbidden},
		{"a grant with no record", host,
			func(st *subStore, _ *memSecrets, p types.ModelProvider) {
				st.grants[0].Spec.Scope = json.RawMessage(`{"host":"` + host + `","secret_name":"` +
					providerSecretName(p.UID, providerKeyPart) + `"}`)
			}, http.StatusForbidden},
		{"a host that is not the provider's", "evil.example", func(*subStore, *memSecrets, types.ModelProvider) {},
			http.StatusForbidden},
		{"the provider was turned off mid-run", host,
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].Disabled = true
			}, http.StatusForbidden},
		{"the provider was re-created under a new UID", host,
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].UID = uuid.NewString()
			}, http.StatusForbidden},
		{"the provider is no longer a Bedrock key provider", host,
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				st.site.ModelProviders.Providers[0].Kind = types.ModelProviderAnthropicAPIKey
			}, http.StatusForbidden},
		{"the run chose another provider", host,
			func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
				other := brBearerProvider()
				other.ID = "other"
				st.site.ModelProviders.Providers = append(st.site.ModelProviders.Providers, other)
				st.run.ModelProviderID = "other"
			}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := brBearerProvider()
			h, st, sec := brAuthored(t, p)
			tc.alter(st, sec, p)
			code, body := resolveBR(t, h, st, tc.host)
			if code != tc.want {
				t.Fatalf("resolve = %d %s, want %d", code, body, tc.want)
			}
			noBRLeak(t, body)
		})
	}

	t.Run("the roster's bedrock-api-key never resolves on a provider run", func(t *testing.T) {
		h, st, _ := brAuthored(t, brBearerProvider())
		raw, _ := json.Marshal(map[string]any{
			"host": host, "header": "Authorization", "format": "Bearer %s", "secret_name": bedrockAPIKeySecret,
			"snapshot": bedrockBearerSnapshotOf(awsSSOScope{}),
		})
		st.grants[0].Spec.Scope = raw
		code, body := resolveBR(t, h, st, host)
		if code != http.StatusForbidden {
			t.Fatalf("resolve = %d %s, want 403", code, body)
		}
		noBRLeak(t, body)
	})
}

func brKeyScope(host, uid, owner string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"host": host, "header": "Authorization", "format": "Bearer %s",
		"secret_name": providerSecretName(uid, providerKeyPart),
		"snapshot":    providerGrantSnapshot{ProviderUID: uid, OwnerSubject: owner},
	})
	return b
}

func TestProviderBedrockSSOSink(t *testing.T) {
	const portal = "portal.sso.us-east-1.amazonaws.com"
	t.Run("resolves the owner's own session", func(t *testing.T) {
		h, st, _ := brAuthored(t, brSSOProvider())
		code, body := resolveBR(t, h, st, portal)
		var resp injectionResponse
		_ = json.Unmarshal([]byte(body), &resp)
		if code != http.StatusOK || resp.Value != brOwnerToken {
			t.Fatalf("resolve = %d %s, want alice's own session", code, body)
		}
	})

	for _, tc := range []struct {
		name  string
		alter func(*subStore, *memSecrets, types.ModelProvider)
		want  string
	}{
		{"the provider re-pinned another account mid-run", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			st.site.ModelProviders.Providers[0].Bedrock.SSOAccountID = "222222222222"
		}, credentialReauthScopeChangedRefusal},
		{"the provider was turned off mid-run", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			st.site.ModelProviders.Providers[0].Disabled = true
		}, credentialReauthScopeChangedRefusal},
		{"a grant recording someone else as the owner", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			brRewriteSSOSnapshot(t, st, func(sn *awsSSOScopeSnapshot) { sn.OwnerSubject = "bob@example.com" })
		}, credentialReauthScopeChangedRefusal},
		{"a roster grant, naming no provider, on a provider run", func(st *subStore, _ *memSecrets, _ types.ModelProvider) {
			brRewriteSSOSnapshot(t, st, func(sn *awsSSOScopeSnapshot) {
				sn.ProviderUID, sn.OwnerSubject, sn.CredentialSource = "", "", string(types.CredentialSourceShared)
			})
		}, credentialReauthScopeChangedRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := brSSOProvider()
			h, st, sec := brAuthored(t, p)
			tc.alter(st, sec, p)
			code, body := resolveBR(t, h, st, portal)
			var eb errorBody
			_ = json.Unmarshal([]byte(body), &eb)
			if code != http.StatusForbidden || eb.Error != tc.want {
				t.Fatalf("resolve = %d %s\nwant 403 carrying %q", code, body, tc.want)
			}
			noBRLeak(t, body)
		})
	}
}

func brRewriteSSOSnapshot(t *testing.T, st *subStore, f func(*awsSSOScopeSnapshot)) {
	t.Helper()
	var scope map[string]any
	var sn struct {
		Snapshot awsSSOScopeSnapshot `json:"snapshot"`
	}
	if json.Unmarshal(st.grants[0].Spec.Scope, &scope) != nil || json.Unmarshal(st.grants[0].Spec.Scope, &sn) != nil {
		t.Fatal("unreadable grant scope")
	}
	f(&sn.Snapshot)
	scope["snapshot"] = sn.Snapshot
	st.grants[0].Spec.Scope, _ = json.Marshal(scope)
}

// TestProviderBedrockCreate is create and Review for a chosen Bedrock
// provider: the MP-6a refusal is lifted, the refusal names the provider and
// why at both doors, and a person with their own key launches.
func TestProviderBedrockCreate(t *testing.T) {
	const admin = "sub-admit-admin"
	for _, tc := range []struct {
		name     string
		p        types.ModelProvider
		own      bool
		want     int
		wantBody string
	}{
		{"bearer, no key of their own", brBearerProvider(), false, http.StatusUnprocessableEntity,
			fmt.Sprintf(mpRunRefusal, "bedrock-key", mpRunNoKey, mpRunRemedySignIn)},
		{"sso, not signed in", brSSOProvider(), false, http.StatusUnprocessableEntity,
			fmt.Sprintf(mpRunRefusal, "bedrock-prod", mpBRNotSignedIn, mpRunRemedySignIn)},
		{"bearer, their own key", brBearerProvider(), true, http.StatusCreated, ""},
		{"sso, signed in", brSSOProvider(), true, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
				srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(tc.p)}, &capStore{}, nil)
				sec := srv.cfg.Secrets.(*memSecrets)
				// The operator's rows under the provider's names never count.
				sec.m[providerSecretName(tc.p.UID, providerKeyPart)] = []byte(brOperatorKey)
				sec.m[providerSecretName(tc.p.UID, providerSSOPart)] = brBlob(brOperatorTok, "123456789012", "BedrockUser", time.Now().Add(time.Hour))
				if tc.own {
					_ = sec.For(admin).Put(context.Background(), providerSecretName(tc.p.UID, providerKeyPart), []byte(brOwnerKey))
					_ = sec.For(admin).Put(context.Background(), providerSecretName(tc.p.UID, providerSSOPart),
						brBlob(brOwnerToken, "123456789012", "BedrockUser", time.Now().Add(time.Hour)))
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
					if run.ModelProviderID != tc.p.ID {
						t.Errorf("model_provider_id = %q, want %s", run.ModelProviderID, tc.p.ID)
					}
				}
			}
		})
	}
}

// TestProviderBedrockSandboxSpecCarriesNoHostAWSCredential is T-13's sandbox
// half: the SandboxSpec of a run on a Bedrock provider — its Mounts, Env and
// SecretEnv, composed from the dispatch plan by the same buildRunMounts and
// splitSecretEnv dispatchRun uses — carries no ~/.aws bind and no SigV4 key,
// though brHarness seeds both on the legacy chain (the operator's host ~/.aws
// dir and static access keys; a session token is added here). The SigV4 check
// is on the operator's key material itself, under any variable name. Every
// Bedrock arm: bearer, and SSO with the session proxy-injected and resident.
func TestProviderBedrockSandboxSpecCarriesNoHostAWSCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      types.ModelProvider
		inject bool
	}{
		{"bedrock_bearer", brBearerProvider(), true},
		{"bedrock_sso, session proxy-injected", brSSOProvider(), true},
		{"bedrock_sso, session resident", brSSOProvider(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, sec := brHarness(t, tc.p)
			h.srv.cfg.AWSSSOProxyInject = tc.inject
			sec.m[bedrockSessionTokenSecret] = []byte("operator-session-must-not-leak")
			var policy types.RunPolicySpec
			env := map[string]string{}
			plan, ok := dispatchSub(h, st, &policy, env, nil)
			if !ok {
				t.Fatalf("dispatch refused: %q", st.failed)
			}
			spec := runner.SandboxSpec{
				Mounts:    buildRunMounts(policy, plan.llm, userMountPosture{}),
				SecretEnv: splitSecretEnv(env, plan.llm.secretEnvKeys),
				Env:       env,
			}
			for _, m := range spec.Mounts {
				if m.Source == h.srv.cfg.BedrockAWSConfigDir || strings.Contains(m.Target, ".aws") {
					t.Errorf("mount %+v: a Bedrock provider run must not bind a host ~/.aws", m)
				}
			}
			sigV4 := []string{bedrockAccessKeyIDSecret, bedrockSecretAccessKeySecret, bedrockSessionTokenSecret}
			for half, m := range map[string]map[string]string{"Env": spec.Env, "SecretEnv": spec.SecretEnv} {
				if v, ok := m["AWS_ACCESS_KEY_ID"]; ok {
					t.Errorf("%s[AWS_ACCESS_KEY_ID] = %q: SigV4 must not reach a Bedrock provider run", half, v)
				}
				for k, v := range m {
					for _, name := range sigV4 {
						if strings.Contains(v, string(sec.m[name])) {
							t.Errorf("%s[%s] carries the operator's %s: SigV4 must not reach a Bedrock provider run", half, k, name)
						}
					}
				}
			}
		})
	}
}
