// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGradeModelCredential pins EVERY row of THREAT-MODEL's resident-secret
// exceptions table that concerns a MODEL credential, plus the never-resident
// closing sentence ("api_key, the Bedrock bearer token …").
//
// The New Run rail repeats what this function graded, so the threat model and
// the console cannot disagree — static copy such as "Minted at launch, injected
// by the proxy. Never written into the sandbox." is false on some deployments. A
// row added to that table with no case here is a row the rail would describe
// wrongly.
//
// Every case folds its lanes through selectedMechanism rather than naming a
// mechanism directly: the residency must be graded from the lane that actually
// resolves, never from the roster's declared enum — case "shared bedrock_bearer
// row, chain falls to the ~/.aws mount" is the trap.
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
		// The one row-fixed case. Under per_user the only admissible lane is the
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

// the two surfaces that publish the grade

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

// TestSetupStatusPublishesOnlyTheRowFixedResidency is the whole of what the
// STATUS surface claims.
//
// A roster is not a resolution: the lane that fires decides residency, and a
// status handler has no request body to resolve one from — New Run sends a
// policy_id or a minimal inline spec, and create folds the run/workspace/default
// integration before any lane is picked. So every row here is SILENT except the
// one the row itself settles, and a console reading this row can be wrong only
// by saying nothing.
func TestSetupStatusPublishesOnlyTheRowFixedResidency(t *testing.T) {
	perUserSSO := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
	}
	for _, tc := range []struct {
		name string
		row  types.AgentProvider
		want string
	}{
		{"per_user bedrock_sso — the one row that settles it", perUserSSO, string(residencySandbox)},
		{"per_user bedrock_sso, DISABLED — launches nothing, says nothing",
			types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
				CredentialSource: types.CredentialSourcePerUser, Disabled: true}, ""},
		// Every shared row's declared lane is satisfied by the whole Bedrock chain
		// (mechanismSatisfied compares the coarse provider type), so the roster
		// cannot tell bearer-at-the-proxy from a fall-through to the resident
		// ~/.aws mount or to static SigV4 keys. Silent.
		{"shared bedrock_bearer", agentRosterRow(types.AgentMechanismBedrockBearer), ""},
		{"shared bedrock_env", agentRosterRow(types.AgentMechanismBedrockEnv), ""},
		{"shared bedrock_sso — the admin's one capture, but the chain may still move", agentRosterRow(types.AgentMechanismBedrockSSO), ""},
		{"shared bedrock_aws_dir", agentRosterRow(types.AgentMechanismBedrockAWSDir), ""},
		{"anthropic_api_key", agentRosterRow(types.AgentMechanismAnthropicAPIKey), ""},
		// The mount-vs-sentinel question is decided by the DAEMON's posture and by
		// whether a resident_host integration actually resolved, neither of which
		// is in the roster.
		{"anthropic_subscription", agentRosterRow(types.AgentMechanismAnthropicSubscription), ""},
		{"none — BYOA", agentRosterRow(types.AgentMechanismNone), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := setupHarnessTools(agentRoster(tc.row), nil)
			var claude SetupHarnessTool
			for _, tool := range tools {
				if tool.ID == "claude-code" {
					claude = tool
				}
			}
			if claude.CredentialResidency != tc.want {
				t.Errorf("credential_residency = %q, want %q", claude.CredentialResidency, tc.want)
			}
		})
	}

	// Legacy mode — no AgentProviders block at all, the compose default. No row,
	// so nothing is settled, so nothing is claimed.
	for _, tool := range setupHarnessTools(types.SiteConfig{}, nil) {
		if tool.CredentialResidency != "" {
			t.Errorf("legacy mode published %+v — there is no row to settle it", tool)
		}
	}
}

// agentRosterRow is a `shared` row for claude-code on the named lane.
func agentRosterRow(m types.AgentMechanism) types.AgentProvider {
	return types.AgentProvider{ID: "claude-code", Mechanism: m, CredentialSource: types.CredentialSourceShared}
}

// TestSetupStatusResidency_PerUserSSOReachesTheWireSignedInOrNot is the FIELD
// CASE, on the DEFAULT path: a member opens New Run, presses nothing, and has to
// read that their AWS sign-in will live inside the sandbox — before they decide
// whether that is acceptable, not after.
//
// BOTH sign-in states, because the answer must not depend on one: the preflight
// twin of the not-signed-in state is a 422 (the declared lane has not resolved),
// which is exactly why this row is the default path and why it is graded from
// the row rather than from a lane.
func TestSetupStatusResidency_PerUserSSOReachesTheWireSignedInOrNot(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "not signed in", true: "signed in"}[signedIn], func(t *testing.T) {
			srv, _ := perUserLoginSrv(t)
			srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
			if signedIn {
				putAWSSSOBlob(t, srv, awsSSOTestFixedNow.Add(time.Hour))
			}
			member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
			row := harnessRow(t, doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", member, ""), "claude-code")
			if row.CredentialResidency != string(residencySandbox) {
				t.Errorf("credential_residency = %q, want %q — a per_user bedrock_sso row is resident "+
					"whether or not this member has signed in yet", row.CredentialResidency, residencySandbox)
			}
		})
	}

	// And the preflight twin of the not-signed-in state: a 422 that publishes no
	// verdict at all. A refusal has none to publish, and pretending otherwise is
	// what would make the status row unnecessary.
	srv, _ := perUserLoginSrv(t)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preflight = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	// The KEY, not the bare word: since 0.7.7 the refusal names its class as
	// `"reason":"model_credential"`, which is not a verdict.
	if strings.Contains(w.Body.String(), `"model_credential":`) {
		t.Errorf("the 422 carries a model_credential: %s", w.Body.String())
	}
}

// TestSetupStatusSaysNothingUnderASubscriptionDeployment: the daemon posture (an
// inject-off deployment blessing the ~/.claude mount in its default policy)
// changes nothing on this surface — there is no lane resolution on a GET at all.
func TestSetupStatusSaysNothingUnderASubscriptionDeployment(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	// No token provider wired, which IS injection off (subscriptionInjectEnabled).
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:  []string{"api.anthropic.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
	}
	srv := New(cfg)
	row := harnessRow(t, do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""), "claude-code")
	if row.CredentialResidency != "" {
		t.Errorf("credential_residency = %q, want empty — the deployment default policy is not the "+
			"body New Run sends, so a GET may not grade one", row.CredentialResidency)
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

	// The STATUS row stays silent — there is no row to settle it — while PREFLIGHT
	// answers precisely, which is the whole shape of the design.
	row := harnessRow(t, do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""), "claude-code")
	if row.CredentialResidency != "" {
		t.Errorf("/setup/status credential_residency = %q, want empty in legacy mode", row.CredentialResidency)
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
		t.Fatalf("preflight model_credential = %+v, want residency %q", got.ModelCredential, residencyProxy)
	}
	// The RESOLVED mechanism rides with it: the rail keys its sentence and its
	// chip on this field, never on the roster's declared one, and in legacy mode
	// there is no declared one to key on at all.
	if got.ModelCredential.Mechanism != string(types.AgentMechanismBedrockBearer) {
		t.Errorf("preflight mechanism = %q, want %q — the lane that resolved",
			got.ModelCredential.Mechanism, types.AgentMechanismBedrockBearer)
	}
}

// TestPreflightResidencyIsOmittedForANonModelRun: task_mode=exec runs a plain
// shell command and is handed no model credential at all, so there is no
// residency to state — omitted, never defaulted. The rail renders no Credentials
// sentence at all for such a run (the screen withholds the agent row too), which
// is the absent-row doctrine rather than a fallback.
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
