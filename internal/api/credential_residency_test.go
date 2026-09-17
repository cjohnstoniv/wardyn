// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGradeModelCredential pins EVERY row of THREAT-MODEL's resident-secret
// exceptions table that concerns a MODEL credential, plus the never-resident
// closing sentence ("api_key, the Bedrock bearer token …").
//
// It exists because the New Run rail used to state residency as unconditional
// static copy: "Minted at launch, injected by the proxy. Never written into the
// sandbox." — false on the very deployment the 0.7.4 field report came from. The
// rail now repeats what this function graded, so the threat model and the
// console can no longer disagree; a row added to that table with no case here is
// a row the rail would describe wrongly.
//
// Every case folds its lanes through selectedMechanism rather than naming a
// mechanism directly: the residency must be graded from the lane that actually
// RESOLVES, never from the roster's declared enum. Trap (b) of the plan's review
// round is case "shared bedrock_bearer row, chain falls to the ~/.aws mount".
func TestGradeModelCredential(t *testing.T) {
	var srv Server
	perUserSSORow := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
	}
	sharedRow := func(m types.AgentMechanism) types.AgentProvider {
		return types.AgentProvider{ID: "claude-code", Mechanism: m, CredentialSource: types.CredentialSourceShared}
	}
	bedrockReady := func(mutate func(*bedrockAuth)) bedrockAuth {
		b := bedrockAuth{ready: true}
		mutate(&b)
		return b
	}

	for _, tc := range []struct {
		name     string
		row      types.AgentProvider
		declared bool
		lanes    llmLanes
		inject   bool
		want     modelCredentialResidency
		staged   bool
	}{{
		// THE ONE ROW-FIXED CASE. Under per_user the only admissible lane is the
		// principal's own captured SSO session (mechanismSatisfied), and that lane
		// is resident — so the answer does not depend on whether they have signed
		// in yet, which is exactly the state the rail has to be honest about.
		name: "per_user bedrock_sso, signed in", row: perUserSSORow, declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.ssoInject = true })},
		want:  residencySandbox,
	}, {
		name: "per_user bedrock_sso, NOT signed in — no lane resolves", row: perUserSSORow, declared: true,
		want: residencySandbox,
	}, {
		name: "shared bedrock_bearer, the bearer lane fires", row: sharedRow(types.AgentMechanismBedrockBearer), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.bearer = true })},
		want:  residencyProxy,
	}, {
		// Trap (b): mechanismSatisfied compares ProviderType only, so this row is
		// SATISFIED by a chain that fell through to the host ~/.aws mount — which
		// is resident. An enum map would have said "proxy" here.
		name: "shared bedrock_bearer row, chain falls to the ~/.aws mount", row: sharedRow(types.AgentMechanismBedrockBearer), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.awsMount = true })},
		want:  residencySandbox,
	}, {
		name: "shared bedrock_bearer row, chain falls to resident SigV4 keys", row: sharedRow(types.AgentMechanismBedrockBearer), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(*bedrockAuth) {})},
		want:  residencySandbox,
	}, {
		name: "shared bedrock_sso — the admin's one capture, every run inherits it", row: sharedRow(types.AgentMechanismBedrockSSO), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.ssoInject = true })},
		want:  residencySandbox,
	}, {
		name: "shared bedrock_aws_dir — WARDYN_BEDROCK_AWS_DIR", row: sharedRow(types.AgentMechanismBedrockAWSDir), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.awsMount = true })},
		want:  residencySandbox,
	}, {
		name: "shared bedrock_env — resident access keys", row: sharedRow(types.AgentMechanismBedrockEnv), declared: true,
		lanes: llmLanes{bedrock: bedrockReady(func(*bedrockAuth) {})},
		want:  residencySandbox,
	}, {
		// The ~/.claude mount with injection ON: the staged file is sanitized to an
		// inert sentinel, so nothing usable is resident — but the sentinel is
		// produced by an operator-run script the daemon never reads back, so the
		// rail says so rather than claiming Wardyn verified it.
		name: "subscription ~/.claude mount, inject ON", row: sharedRow(types.AgentMechanismAnthropicSubscription), declared: true,
		lanes: llmLanes{subscription: true}, inject: true,
		want: residencyProxy, staged: true,
	}, {
		// WARDYN_SUBSCRIPTION_INJECT=off — the compose stack's own default. A real,
		// refreshable copy of the operator's OAuth credentials lives in the sandbox.
		name: "subscription ~/.claude mount, inject OFF", row: sharedRow(types.AgentMechanismAnthropicSubscription), declared: true,
		lanes: llmLanes{subscription: true},
		want:  residencySandbox,
	}, {
		// The managed setup-token lane has no resident copy to be off about: the
		// sandbox holds the sentinel and the proxy injects the live token.
		name: "subscription via the managed setup-token, no mount", row: sharedRow(types.AgentMechanismAnthropicSubscription), declared: true,
		lanes: llmLanes{managed: true},
		want:  residencyProxy,
	}, {
		name: "anthropic_api_key", row: sharedRow(types.AgentMechanismAnthropicAPIKey), declared: true,
		lanes: llmLanes{apiKey: true},
		want:  residencyProxy,
	}, {
		name: "openai_api_key", row: types.AgentProvider{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey}, declared: true,
		lanes: llmLanes{apiKey: true},
		want:  residencyProxy,
	}, {
		// BYOA: Wardyn wires nothing, so it cannot say where the image's own
		// credential lives — and must not imply the proxy holds it.
		name: "none row — BYOA", row: sharedRow(types.AgentMechanismNone), declared: true,
		want: residencyImage,
	}, {
		name: "a declared lane that has not resolved yet", row: sharedRow(types.AgentMechanismAnthropicAPIKey), declared: true,
		want: residencyUnknown,
	}, {
		// Legacy mode (no roster — the compose default) has no row at all, so it
		// grades from the resolved lanes exactly like a declared deployment.
		name:  "legacy, no roster, captured SSO resolves",
		lanes: llmLanes{bedrock: bedrockReady(func(b *bedrockAuth) { b.ssoInject = true })},
		want:  residencySandbox,
	}, {
		name:  "legacy, no roster, api key resolves",
		lanes: llmLanes{apiKey: true},
		want:  residencyProxy,
	}, {
		name: "legacy, no roster, nothing resolves",
		want: residencyUnknown,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.row.ID
			if agent == "" {
				agent = "claude-code"
			}
			selected, ok := srv.selectedMechanism(agent, tc.lanes.subscription, tc.lanes.bedrock,
				tc.lanes.managed, tc.lanes.apiKey)
			got := gradeModelCredential(tc.row, tc.declared, tc.lanes, selected, ok, tc.inject)
			if got.Residency != tc.want {
				t.Errorf("residency = %q, want %q (selected=%q ok=%v)", got.Residency, tc.want, selected, ok)
			}
			if got.StagedPlaceholder != tc.staged {
				t.Errorf("staged_placeholder = %v, want %v", got.StagedPlaceholder, tc.staged)
			}
			if tc.declared && got.CredentialSource != string(tc.row.CredentialSource) {
				t.Errorf("credential_source = %q, want %q", got.CredentialSource, tc.row.CredentialSource)
			}
		})
	}
}

// TestGradeModelCredentialNeverDefaultsToProxy is the negative control the whole
// lane turns on: the old rail said "injected by the proxy; never written into
// the sandbox" with nothing resolved at all. The zero input must grade UNKNOWN,
// so the rail's proxy sentence can never be reached by an absence.
func TestGradeModelCredentialNeverDefaultsToProxy(t *testing.T) {
	if got := gradeModelCredential(types.AgentProvider{}, false, llmLanes{}, "", false, false); got.Residency != residencyUnknown {
		t.Errorf("the zero grade = %q, want %q — an absence must never render a residency claim",
			got.Residency, residencyUnknown)
	}
}

// ── the two surfaces that publish the grade ─────────────────────────────────

// harnessRow reads ONE agent's row out of a /setup/status body.
func harnessRow(t *testing.T, w *httptest.ResponseRecorder, agent string) SetupHarnessTool {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("/setup/status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var st SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode /setup/status: %v", err)
	}
	for _, h := range st.Harnesses {
		if h.ID == agent {
			return h
		}
	}
	t.Fatalf("no %q row in /setup/status harnesses %+v", agent, st.Harnesses)
	return SetupHarnessTool{}
}

// TestSetupStatusResidency_PerUserSSOMemberWhoHasNotSignedIn is the FIELD CASE,
// on the DEFAULT path: a member opens New Run, presses nothing, and has to read
// that their AWS sign-in will live inside the sandbox — before they decide
// whether that is acceptable, not after.
//
// The preflight twin of this exact state is a 422 (the declared lane has not
// resolved because they have not signed in), which is why the status row is the
// default path and preflight only overrides it.
func TestSetupStatusResidency_PerUserSSOMemberWhoHasNotSignedIn(t *testing.T) {
	srv, _ := perUserLoginSrv(t)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	row := harnessRow(t, doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", member, ""), "claude-code")
	if row.CredentialResidency != string(residencySandbox) {
		t.Errorf("credential_residency = %q, want %q — a per_user bedrock_sso row is resident "+
			"whether or not this member has signed in yet", row.CredentialResidency, residencySandbox)
	}

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member,
		`{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preflight = %d, want 422 — the declared lane has not resolved; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "model_credential") {
		t.Errorf("the 422 carries a model_credential: %s — a refusal has no verdict to publish", w.Body.String())
	}
}

// TestSetupStatusResidency_SharedBearerRowFallingToTheAWSMount is review trap
// (b): mechanismSatisfied compares only the coarse provider type, so a declared
// bedrock_bearer row is SATISFIED by a chain that fell through to the host
// ~/.aws mount — which is resident. A console that mapped the roster enum to a
// sentence would say "never written into the sandbox" here.
func TestSetupStatusResidency_SharedBearerRowFallingToTheAWSMount(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{
		govEscapeStore: newGovEscapeStore(&capStore{}),
		site:           agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer}),
	})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}} // no bedrock-api-key: the bearer lane cannot fire
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.BedrockAWSConfigDir = t.TempDir()
	srv := New(cfg)

	row := harnessRow(t, do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""), "claude-code")
	if row.CredentialResidency != string(residencySandbox) {
		t.Errorf("credential_residency = %q, want %q — the declared bearer lane did not fire; the ~/.aws "+
			"mount did, and the AWS SDK signs SigV4 inside the sandbox", row.CredentialResidency, residencySandbox)
	}
}

// TestSetupStatusResidency_SubscriptionWithInjectOff: THREAT-MODEL's
// "Subscription ~/.claude mount, WARDYN_SUBSCRIPTION_INJECT=off only" row, whose
// own note is that the COMPOSE stack defaults the variable to off — so on that
// stack a real, refreshable copy of the operator's OAuth credentials is resident
// BY DEFAULT and the rail must say so.
func TestSetupStatusResidency_SubscriptionWithInjectOff(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	// The deployment default policy blesses the ~/.claude mount; no token
	// provider is wired, which IS injection off (subscriptionInjectEnabled).
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:  []string{"api.anthropic.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
	}
	srv := New(cfg)

	row := harnessRow(t, do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""), "claude-code")
	if row.CredentialResidency != string(residencySandbox) {
		t.Errorf("credential_residency = %q, want %q — with no proxy-side token provider the mount IS "+
			"the credential", row.CredentialResidency, residencySandbox)
	}
	if row.StagedPlaceholder {
		t.Errorf("staged_placeholder = true with injection off — the staged file is the real credential there")
	}
}

// TestResidencyInLegacyModeGradesFromTheResolvedLanes: no AgentProviders block
// at all — the compose default — so there is no declared enum to read. The grade
// comes from the lane that resolves, on BOTH surfaces.
func TestResidencyInLegacyModeGradesFromTheResolvedLanes(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	cfg.Secrets = &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	row := harnessRow(t, do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""), "claude-code")
	if row.CredentialResidency != string(residencyProxy) {
		t.Errorf("/setup/status credential_residency = %q, want %q — the bearer lane resolves and the "+
			"proxy substitutes it on the wire", row.CredentialResidency, residencyProxy)
	}

	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if got.ModelCredential == nil || got.ModelCredential.Residency != residencyProxy {
		t.Errorf("preflight model_credential = %+v, want residency %q", got.ModelCredential, residencyProxy)
	}
}

// TestPreflightResidencyIsOmittedForANonModelRun: task_mode=exec runs a plain
// shell command and is handed no model credential at all, so there is no
// residency to state — omitted, never defaulted (the rail then shows nothing
// rather than the proxy sentence).
func TestPreflightResidencyIsOmittedForANonModelRun(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	cfg.Secrets = &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken,
		`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "model_credential") {
		t.Errorf("an exec run's preflight carries a model_credential: %s", w.Body.String())
	}
}
