// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file pins enforceConfiguredLLMMechanism: the ONE site that keeps a
// declared model-access lane from being silently replaced by another provider's
// credential.
//
// The interesting cases are NOT reachable from a hand-built llmTransport,
// because the failure the gate exists for is a PRECEDENCE accident: a
// deployment whose row says "api key" resolves BEDROCK, and a readiness check
// would have admitted it (the declared lane really is ready; it just is not the
// one that won). So the matrix below drives the real resolveLLMTransport over a
// credential environment and hands its verdict to the gate — the same pair of
// steps dispatch takes.

// mechanismGateStore is enforceConfiguredLLMMechanism's refusal path (failAndRevoke
// CASes STARTING -> FAILED) plus the site config enforceCreateLLMMechanism reads.
type mechanismGateStore struct {
	store.Store
	sc     types.SiteConfig
	failed bool
}

func (s *mechanismGateStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	if to == types.RunFailed {
		s.failed = true
	}
	return true, nil
}

func (s *mechanismGateStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}

// agentRoster is a SiteConfig carrying exactly these agent rows.
func agentRoster(rows ...types.AgentProvider) types.SiteConfig {
	return types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: rows}}
}

// bedrockBearerCfg / bedrockStaticCfg / managedCfg are the credential
// environments the matrix resolves a real transport from.
func bedrockBearerCfg() Config {
	return Config{
		BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets:      &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}},
		MaskRegistry: secretmask.NewRegistry(),
	}
}

func bedrockStaticCfg() Config {
	return Config{
		BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets: &memSecrets{m: map[string][]byte{
			bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
			bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
		}},
		MaskRegistry: secretmask.NewRegistry(),
	}
}

func anthropicKeyInjection() []runner.InjectionGrant {
	return []runner.InjectionGrant{{
		GrantID: uuid.New(),
		Rule:    egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: "anthropic-api-key"},
	}}
}

// TestEnforceConfiguredLLMMechanism_SelectedNotDeclared is the lane's whole
// point, driven end to end through resolveLLMTransport.
func TestEnforceConfiguredLLMMechanism_SelectedNotDeclared(t *testing.T) {
	cases := []struct {
		name        string
		agent       string
		cfg         Config
		injections  []runner.InjectionGrant
		mounts      []types.WorkspaceMount
		interactive bool
		taskMode    string
		task        string
		sc          types.SiteConfig
		wantRefused bool
	}{
		// ROUND-8 CONFIG 1: the row says api key, and Bedrock + a bearer secret
		// are also configured, so resolveLLMTransport dispatches BEDROCK. The
		// declared lane is READY (the api-key grant is right there) — only a
		// SELECTED-vs-declared comparison catches this.
		{
			name: "api-key declared, bedrock bearer dispatches", agent: "claude-code",
			cfg: bedrockBearerCfg(), injections: anthropicKeyInjection(),
			sc:          agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
			wantRefused: true,
		},
		// ROUND-8 CONFIG 2: the row says subscription, a managed token is
		// connected, and an api-key grant exists — which makes managed OPT OUT,
		// so the run dispatches on the api key and the subscription is dropped.
		{
			name: "subscription declared, api-key dispatches", agent: "claude-code",
			cfg: Config{
				ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}},
				Secrets:      &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}},
			},
			injections: anthropicKeyInjection(),
			sc: agentRoster(types.AgentProvider{
				ID: "claude-code", Mechanism: types.AgentMechanismAnthropicSubscription,
				CredentialSource: types.CredentialSourceShared,
			}),
			wantRefused: true,
		},
		// Nothing configured at all: the api-key arm is resolveLLMTransport's
		// unconditional final else, so the placeholder env is NOT a credential —
		// nothing was selected, and a declared lane is not carrying the run.
		{
			name: "api-key declared, nothing credentials the run", agent: "claude-code",
			sc:          agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
			wantRefused: true,
		},
		// The matching cases: each declared lane is the one that fires.
		{
			name: "bedrock bearer declared and selected", agent: "claude-code", cfg: bedrockBearerCfg(),
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer}),
		},
		// The COARSE fold under `shared`: the admin declared the captured-SSO
		// sub-lane, Bedrock's own credential chain fell through to the operator's
		// static keys. Within one mechanism that chain is today's behaviour and
		// must stay admitted — the credential's SOURCE moved, its mechanism did not.
		{
			name: "bedrock sso declared, static keys selected (folded)", agent: "claude-code", cfg: bedrockStaticCfg(),
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}),
		},
		{
			name: "api-key declared and selected", agent: "claude-code", injections: anthropicKeyInjection(),
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
		},
		{
			name: "subscription declared, resident mount selected", agent: "claude-code",
			cfg:    Config{SubscriptionToken: fakeSubProvider{tok: subscription.Token{Value: "resident-tok"}}},
			mounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
			sc:     agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicSubscription}),
		},
		{
			name: "subscription declared, managed selected", agent: "claude-code",
			cfg: Config{ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}},
			sc:  agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicSubscription}),
		},
		// Vendor-agnostic: codex-cli's api-key arm is the OpenAI one.
		{
			name: "openai api-key declared and selected", agent: "codex-cli",
			injections: []runner.InjectionGrant{{
				GrantID: uuid.New(),
				Rule:    egress.InjectionRule{Host: "api.openai.com", Header: "Authorization", Format: "Bearer %s", SecretName: "openai-api-key"},
			}},
			sc: agentRoster(types.AgentProvider{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey}),
		},
		// An INTERACTIVE model run is refused too: nobody can repair a model
		// credential from inside the sandbox.
		{
			name: "interactive run refused", agent: "claude-code", interactive: true, cfg: bedrockBearerCfg(),
			sc:          agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
			wantRefused: true,
		},
		// A row for ANOTHER agent says nothing about this run.
		{
			name: "row for a different agent", agent: "claude-code", cfg: bedrockBearerCfg(),
			sc: agentRoster(types.AgentProvider{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey}),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			st := &mechanismGateStore{}
			cfg := c.cfg
			cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
			cfg.SubscriptionPostureOK = true
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), Agent: c.agent, Task: c.task, State: types.RunStarting}
			policy := &types.RunPolicySpec{AllowedDomains: []string{"git.example.com"}, WorkspaceMounts: c.mounts}
			llm := srv.resolveLLMTransport(context.Background(), run, policy, map[string]string{},
				c.injections, c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil, awsSSOScope{})

			admitted := srv.enforceConfiguredLLMMechanism(context.Background(), run, c.sc, llm, c.injections)
			if admitted == c.wantRefused {
				t.Fatalf("admitted = %v, want refused = %v", admitted, c.wantRefused)
			}
			if st.failed != c.wantRefused {
				t.Fatalf("run failed = %v, want %v", st.failed, c.wantRefused)
			}
		})
	}
}

// TestEnforceConfiguredLLMMechanism_LegacyModeRefusesNothing is the round-11
// constraint stated as a test: with no AgentProviders block, EVERY credential
// environment the transport golden covers still dispatches. Eleven existing
// ./internal/api tests plus the PG-lane
// TestDispatch_BedrockAbsentCreds_FallsBackToAPIKeyPlaceholder depend on it —
// dispatch fixtures seed no model credential, and an early FAILED run cascades
// (it stops counting toward the concurrency cap, it never reaches the
// completion watcher).
func TestEnforceConfiguredLLMMechanism_LegacyModeRefusesNothing(t *testing.T) {
	for _, c := range llmGoldenCases() {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			st := &mechanismGateStore{}
			cfg := c.cfg
			cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
			cfg.SubscriptionPostureOK = true
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), Agent: c.agent, Task: c.task, State: types.RunStarting, WorkspaceID: c.workspaceID}
			policy := &types.RunPolicySpec{AllowedDomains: []string{"git.example.com"}, WorkspaceMounts: c.mounts}
			llm := srv.resolveLLMTransport(context.Background(), run, policy, map[string]string{},
				c.injections, c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil, awsSSOScope{})

			// No block at all, and a block whose rows name other agents: both are
			// legacy for this run.
			for name, sc := range map[string]types.SiteConfig{
				"no block":        {},
				"rows elsewhere":  agentRoster(types.AgentProvider{ID: "my-image", Mechanism: types.AgentMechanismNone}),
				"empty row slice": {AgentProviders: &types.AgentProviders{}},
			} {
				if !srv.enforceConfiguredLLMMechanism(context.Background(), run, sc, llm, c.injections) {
					t.Fatalf("%s: refused a run in legacy open mode", name)
				}
				if st.failed {
					t.Fatalf("%s: failed a run in legacy open mode", name)
				}
			}
		})
	}
}

// TestEnforceConfiguredLLMMechanism_UngatedRunKinds: the three-term gate. A run
// that makes no model call, a credential-capture box, and an agent Wardyn wires
// no model credential for are all admitted EVEN under a declared row that
// nothing could satisfy — the last one is why `--agent none` and every
// WARDYN_AGENT_IMAGES custom image still dispatch.
func TestEnforceConfiguredLLMMechanism_UngatedRunKinds(t *testing.T) {
	wsID := uuid.New()
	cases := map[string]struct {
		agent       string
		task        string
		taskMode    string
		workspaceID *uuid.UUID
		sc          types.SiteConfig
	}{
		"exec run": {agent: "claude-code", taskMode: "exec",
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})},
		"scan run (workspace-bound, non-interactive)": {agent: "claude-code", workspaceID: &wsID,
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})},
		"harness login box": {agent: "claude-code", task: harnessLoginTask,
			sc: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})},
		"--agent none under a none row": {agent: "none",
			sc: agentRoster(types.AgentProvider{ID: "none", Mechanism: types.AgentMechanismNone})},
		"custom image under a none row": {agent: "my-image",
			sc: agentRoster(types.AgentProvider{ID: "my-image", Mechanism: types.AgentMechanismNone})},
		// The pathological rows a write boundary refuses but a hand-edited
		// JSONB could still hold: no lane can fire for these agents, so the
		// gate must not read that as "dead".
		"custom image under a bedrock row": {agent: "my-image",
			sc: agentRoster(types.AgentProvider{ID: "my-image", Mechanism: types.AgentMechanismBedrockSSO})},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			st := &mechanismGateStore{}
			cfg := bedrockBearerCfg()
			cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
			cfg.AgentImages = map[string]string{"my-image": "example.com/my-image:v1"}
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), Agent: c.agent, Task: c.task, State: types.RunStarting, WorkspaceID: c.workspaceID}
			policy := &types.RunPolicySpec{AllowedDomains: []string{"git.example.com"}}
			llm := srv.resolveLLMTransport(context.Background(), run, policy, map[string]string{},
				nil, false, c.taskMode, "http://wardyn-proxy:3128", nil, awsSSOScope{})

			if !srv.enforceConfiguredLLMMechanism(context.Background(), run, c.sc, llm, nil) {
				t.Fatal("an ungated run kind was refused")
			}
			if st.failed {
				t.Error("an ungated run kind was failed")
			}
		})
	}
}

// TestMechanismSatisfied_PerUserAdmitsOnlyTheSSOLane pins the per_user half,
// which cannot be reached through a credential environment yet: under per_user
// the ONLY admissible lane is a captured AWS SSO session, because every other
// Bedrock arm is an operator-namespace read.
//
// The resolver now keeps the same promise at the source: under per_user it
// reads only the principal's OWN blob and skips the bearer/mount/static arms
// entirely, so a member with no session of their own arrives here as "nothing
// selected" — the row below — rather than carrying the admin's session on a
// lane that fired underneath this predicate. That end of it is pinned by
// TestResolveBedrockAuth_PerUserNeverReadsTheOperatorRow (modelaccess_test.go);
// this table pins the predicate itself.
func TestMechanismSatisfied_PerUserAdmitsOnlyTheSSOLane(t *testing.T) {
	perUser := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	}
	cases := map[string]struct {
		selected types.AgentMechanism
		ok       bool
		want     bool
	}{
		"operator static keys":   {types.AgentMechanismBedrockEnv, true, false},
		"operator bearer key":    {types.AgentMechanismBedrockBearer, true, false},
		"host ~/.aws mount":      {types.AgentMechanismBedrockAWSDir, true, false},
		"nothing selected":       {"", false, false},
		"a captured SSO session": {types.AgentMechanismBedrockSSO, true, true},
	}
	for name, c := range cases {
		if got := mechanismSatisfied(perUser, c.selected, c.ok); got != c.want {
			t.Errorf("%s: mechanismSatisfied = %v, want %v", name, got, c.want)
		}
	}
	// Under `shared`, the same three operator lanes are the Bedrock chain and
	// stay admitted — the fold is what makes today's behaviour survive.
	shared := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}
	for _, m := range []types.AgentMechanism{
		types.AgentMechanismBedrockEnv, types.AgentMechanismBedrockBearer, types.AgentMechanismBedrockAWSDir,
	} {
		if !mechanismSatisfied(shared, m, true) {
			t.Errorf("shared: %s must fold onto the declared bedrock lane", m)
		}
	}
	// A `none` row matches only when nothing fired.
	none := types.AgentProvider{ID: "none", Mechanism: types.AgentMechanismNone}
	if !mechanismSatisfied(none, "", false) || mechanismSatisfied(none, types.AgentMechanismBedrockBearer, true) {
		t.Error("a none row must match exactly the no-lane-fired case")
	}
}

// TestLLMMechanismRefusal_NamesBothLanes: the refusal says what was configured,
// what state it is in, and — when a DIFFERENT lane took the run — which one, so
// an admin is not sent looking for a missing credential that is sitting there
// working. A renewable captured SSO session that could not be renewed carries
// C1's own sentence instead, which names WHY.
func TestLLMMechanismRefusal_NamesBothLanes(t *testing.T) {
	row := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}
	msg := llmMechanismRefusal(row, "", false, "")
	for _, want := range []string{
		llmMechanismWords[types.AgentMechanismBedrockSSO],
		llmMechanismStateNotConfigured,
		"Wardyn does not substitute a different model provider.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q is missing %q", msg, want)
		}
	}
	// A MISMATCH names both lanes and never claims the declared one is missing.
	apiKeyRow := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}
	mismatch := llmMechanismRefusal(apiKeyRow, types.AgentMechanismBedrockBearer, true, "")
	for _, want := range []string{
		llmMechanismWords[types.AgentMechanismAnthropicAPIKey],
		llmMechanismWords[types.AgentMechanismBedrockBearer],
	} {
		if !strings.Contains(mismatch, want) {
			t.Errorf("mismatch refusal %q is missing %q", mismatch, want)
		}
	}
	if strings.Contains(mismatch, llmMechanismStateNotConfigured) {
		t.Errorf("mismatch refusal %q calls a working credential unconfigured", mismatch)
	}
	spent := awsSSORefreshSpentRefusal(false)
	if got := llmMechanismRefusal(row, "", false, spent); got != spent {
		t.Errorf("a renewal failure must carry its own sentence, got %q", got)
	}
	// Another declared lane's refusal must not borrow the SSO sentence.
	if got := llmMechanismRefusal(apiKeyRow, "", false, spent); got == spent {
		t.Error("an api-key row must not report an AWS SSO renewal failure")
	}
	// Every closed mechanism has words: a refusal must never name an empty lane.
	for m := range types.ClosedAgentMechanisms {
		if llmMechanismWords[m] == "" {
			t.Errorf("mechanism %q has no words for a refusal", m)
		}
	}
}

// TestEnforceCreateLLMMechanism_RefusesBeforeARunExists is the create/Review
// half: the same declaration, the same sentence, answered as a 422 — and, in
// legacy open mode, nothing refused at all.
func TestEnforceCreateLLMMechanism_RefusesBeforeARunExists(t *testing.T) {
	cases := map[string]struct {
		req         createRunRequest
		sc          types.SiteConfig
		wantRefused bool
	}{
		"declared api key, bedrock would dispatch": {
			req:         createRunRequest{Agent: "claude-code", Task: "ship it"},
			sc:          agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
			wantRefused: true,
		},
		"declared bedrock, bedrock dispatches": {
			req: createRunRequest{Agent: "claude-code", Task: "ship it"},
			sc:  agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer}),
		},
		// Interactive is refused at create too (the advisory never fired for it).
		"declared api key, interactive": {
			req:         createRunRequest{Agent: "claude-code", Interactive: true},
			sc:          agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
			wantRefused: true,
		},
		"legacy open mode": {
			req: createRunRequest{Agent: "claude-code", Task: "ship it"},
			sc:  types.SiteConfig{},
		},
		"exec run": {
			req: createRunRequest{Agent: "claude-code", TaskMode: "exec"},
			sc:  agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			st := &mechanismGateStore{sc: c.sc}
			cfg := bedrockBearerCfg()
			cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
			srv := New(cfg)
			rec := httptest.NewRecorder()

			ok := srv.enforceCreateLLMMechanism(context.Background(), rec, c.req, types.RunPolicySpec{}, nil, "", nil, true)
			if ok == c.wantRefused {
				t.Fatalf("admitted = %v, want refused = %v (body %q)", ok, c.wantRefused, rec.Body.String())
			}
			if !c.wantRefused {
				return
			}
			if rec.Code != 422 {
				t.Errorf("status = %d, want 422", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "does not substitute a different model provider") {
				t.Errorf("body = %q, want the declared-mechanism refusal", rec.Body.String())
			}
			// The class the console acts on: the New Run rail opens the AWS
			// sign-in on it and launches again once the capture lands.
			var body struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Reason != llmRefusalAuditReason {
				t.Errorf("reason = %q (err %v), want %q", body.Reason, err, llmRefusalAuditReason)
			}
		})
	}
}

// TestCreateRunAuditData_CarriesClampWarnings: the run-bound run.create success
// datum is where the tightening list lives (the run-detail "Effective policy"
// widget's only join key), and it is absent — not empty — when nothing narrowed.
func TestCreateRunAuditData_CarriesClampWarnings(t *testing.T) {
	warns := []string{"resources capped to operator maximum", `dropped 1 egress domain(s) not in operator allowlist: ["evil.example"]`}
	data := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), types.ConfinementClass("CC2"), "jti", warns, false)
	got, ok := data["clamp_warnings"].([]string)
	if !ok || len(got) != len(warns) || got[0] != warns[0] {
		t.Fatalf("clamp_warnings = %#v, want %#v", data["clamp_warnings"], warns)
	}
	if _, present := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), types.ConfinementClass("CC2"), "jti", nil, false)["clamp_warnings"]; present {
		t.Error("clamp_warnings must be absent when launch narrowed nothing")
	}
}

// TestCreateRunAuditData_CredentialConfinement is #150: the closed-vocabulary
// credential_confinement field is present, with the ONE value it carries
// today, exactly when the caller says this run's SSO-delivered credential is
// below the confinement floor — and absent otherwise, never published as a
// false negative.
func TestCreateRunAuditData_CredentialConfinement(t *testing.T) {
	req := createRunRequest{Agent: "claude-code"}
	data := createRunAuditData(req, nil, types.CC1, "", "jti", nil, true)
	if got := data["credential_confinement"]; got != credentialConfinementBelowFloor {
		t.Errorf("credential_confinement = %v, want %q", got, credentialConfinementBelowFloor)
	}
	if _, present := createRunAuditData(req, nil, types.CC3, "", "jti", nil, false)["credential_confinement"]; present {
		t.Error("credential_confinement must be absent when the caller reports no below-floor advisory")
	}
}

// TestCreateRunAuditData_ConfinementSource is 0.7.8: reqCC ("" for an
// unspecified request) is what decides confinement_source, never enforced —
// enforced alone cannot tell a caller who asked for CC1 apart from a runner
// that only had CC1 to offer, and the audit row is the one place that
// distinction survives (docs/AUDIT-ACTIONS.md's run.create row).
func TestCreateRunAuditData_ConfinementSource(t *testing.T) {
	req := createRunRequest{Agent: "claude-code"}
	if got := createRunAuditData(req, nil, types.CC1, "", "jti", nil, false)["confinement_source"]; got != "defaulted" {
		t.Errorf("confinement_source = %v, want \"defaulted\" for an empty reqCC", got)
	}
	if got := createRunAuditData(req, nil, types.CC1, types.CC1, "jti", nil, false)["confinement_source"]; got != "requested" {
		t.Errorf("confinement_source = %v, want \"requested\" when the caller named CC1 explicitly", got)
	}
}

// pgRosterSrv is a PG-backed server driving the REAL POST /runs door with an
// agent roster stored the way an admin's PUT stores it. site_config is a
// store-wide singleton, so the roster is restored to the zero value afterward —
// a value seeded here otherwise leaks into every other test sharing
// WARDYN_TEST_PG.
func pgRosterSrv(t *testing.T, fr *fakeRunner, row types.AgentProvider) *Server {
	t.Helper()
	srv, _ := pgHarnessWithRunner(t, fr)
	ctx := context.Background()
	if _, err := srv.cfg.Store.PutSiteConfig(ctx, types.SiteConfig{
		AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{row}},
	}); err != nil {
		t.Fatalf("seed agent roster: %v", err)
	}
	t.Cleanup(func() {
		if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}); err != nil {
			t.Errorf("restore site config: %v", err)
		}
	})
	return srv
}

// TestRosterRun_ManagedLaneFoldsTheSameAtCreateAndDispatch is the regression for
// the two halves of one bug: create resolved the MANAGED subscription lane on
// different terms than dispatch, and selectedMechanism tests subscription before
// Bedrock, so the two ends disagreed about what a run would dispatch on.
//
// Both arms are driven end to end — POST /runs, then the spec the runner was
// actually handed — because that disagreement is invisible from either half
// alone: each side's own unit test passed the whole time.
func TestRosterRun_ManagedLaneFoldsTheSameAtCreateAndDispatch(t *testing.T) {
	// (a) BEDROCK WINS OVER MANAGED. A managed token is connected AND Bedrock is
	// configured with a bearer key, under a bedrock_bearer row. Dispatch suppresses
	// managed (!bedrockReady) and dispatches Bedrock — so create must admit. With
	// create's copy of the managed terms missing that condition, it folded the run
	// onto the subscription lane and 422'd every model run at the door.
	t.Run("bedrock configured under a bedrock row still launches", func(t *testing.T) {
		fr := &fakeRunner{}
		srv := pgRosterSrv(t, fr, types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer})
		srv.cfg.SubscriptionPostureOK = true
		srv.cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}
		srv.cfg.BedrockRegion = "us-east-1"
		srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
		srv.cfg.Secrets = &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}}
		srv.cfg.MaskRegistry = secretmask.NewRegistry()

		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201 — dispatch would have credentialed this run; body=%s", w.Code, w.Body.String())
		}
		fr.waitForSandbox(t)
		if fr.createCalls != 1 {
			t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
		}
		if fr.lastSpec.Env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
			t.Errorf("the run dispatched on %v, want the Bedrock lane its row declares", fr.lastSpec.Env)
		}
	})

	// (b) POSTURE. Every multi-user/SSO deployment — the only kind with a roster —
	// runs SubscriptionPostureOK=false, where dispatch refuses to serve the
	// operator's own subscription to a member. Create used to ignore that and read
	// "managed" as the lane, admitting a subscription-row run that then reached
	// dispatch with nothing: 201, then FAILED with no sandbox — the boot-and-die
	// this gate exists to prevent. It must be refused AT THE DOOR instead.
	t.Run("off-posture managed is refused at the door, never launched", func(t *testing.T) {
		fr := &fakeRunner{}
		srv := pgRosterSrv(t, fr, types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismAnthropicSubscription,
			CredentialSource: types.CredentialSourceShared,
		})
		srv.cfg.SubscriptionPostureOK = false
		srv.cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}

		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "does not substitute a different model provider") {
			t.Errorf("body = %q, want the declared-mechanism refusal", w.Body.String())
		}
		if fr.createCalls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0 — a refused run boots nothing", fr.createCalls)
		}
	})
}

// TestRecordLaunchRefusedMintsNothing pins the OTHER half of the promise, on the
// one production path that still reaches the DISPATCH refusal: a record session
// (newStepRun bypasses run create entirely, and it is an interactive MODEL run).
// The gate sits ahead of the MITM CA and every grant author, so a refused run
// must reach no sandbox at all and carry no credential in any env it composed.
func TestRecordLaunchRefusedMintsNothing(t *testing.T) {
	fr := &fakeRunner{}
	// A row declaring a lane this deployment has no credential for: nothing is
	// selected, so the declared mechanism is not carrying the run.
	srv := pgRosterSrv(t, fr, types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO})
	ctx := context.Background()
	// A unique name: workspaces.name is UNIQUE store-wide and every PG-backed
	// test in this package shares one database.
	ws, err := srv.cfg.Store.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "c2-record-" + uuid.NewString(), Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	run, _, err := srv.launchRecordRun(ctx, "admin@corp.example", ws, "capture", "capture", false)
	if err != nil {
		t.Fatalf("launchRecordRun err = %v — the roster admits this agent; the refusal is the run's, not the launcher's", err)
	}
	if run.State != types.RunFailed {
		t.Errorf("run state = %q, want FAILED — the declared lane is not carrying this run", run.State)
	}
	if fr.createCalls != 0 {
		t.Fatalf("CreateSandbox calls = %d, want 0 — a refused run mints nothing", fr.createCalls)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if v, ok := fr.lastSpec.Env[k]; ok {
			t.Errorf("a refused run composed %s=%q", k, v)
		}
	}
}

// TestDispatch_UnreadableRosterRefusesTheCredential (V1-r2 fix-s2 review R-02).
//
// The scope that decides WHOSE captured AWS SSO session a run is served — and
// whether the operator-wide Bedrock bearer key is reachable at all — is resolved
// at dispatch from the site config (`awsSSOScopeFor(siteCfg, …)`,
// runs_dispatch_llm.go). `runs_dispatch.go` reads that config with an error it
// then never consults on this path, so a store blip yielded a ZERO SiteConfig:
// perUser=false, owner="" — the OPERATOR namespace — and a per_user member's run
// was credentialed with the deployment-wide session. That is S2-01/S2-08's
// fail-open on the SERVING door.
//
// Refused, not degraded: a credential must never silently change source.
func TestDispatch_UnreadableRosterRefusesTheCredential(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg         Config
		task        string
		taskMode    string
		agent       string
		mounts      []types.WorkspaceMount
		siteCfgOK   bool
		wantRefused bool
	}{
		"a claude-code model run whose roster read failed": {
			cfg: bedrockBearerCfg(), agent: "claude-code", wantRefused: true,
		},
		"the same run on a roster that read": {
			cfg: bedrockBearerCfg(), agent: "claude-code", siteCfgOK: true,
		},
		"a login box has no credential to be given": {
			cfg: bedrockBearerCfg(), agent: "claude-code", task: harnessLoginTask,
		},
		"an exec run signs no model request": {
			cfg: bedrockBearerCfg(), agent: "claude-code", taskMode: "exec",
		},
		"a deployment with no Bedrock model configured": {
			cfg:   Config{Secrets: &memSecrets{m: map[string][]byte{}}, MaskRegistry: secretmask.NewRegistry()},
			agent: "claude-code",
		},
		"an agent this lane never credentials": {
			cfg: bedrockBearerCfg(), agent: "codex-cli",
		},
		// The host-staged subscription outranks Bedrock in resolveBedrockAuth's
		// own precedence, so this run was never going to be served from the
		// roster's namespace at all: it brings its own credential.
		"a subscription run brings its own credential": {
			cfg: bedrockBearerCfg(), agent: "claude-code",
			mounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			st := &mechanismGateStore{}
			cfg := tc.cfg
			cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
			srv := New(cfg)
			run := types.AgentRun{ID: uuid.New(), Agent: tc.agent, Task: tc.task, State: types.RunStarting}
			policy := &types.RunPolicySpec{WorkspaceMounts: tc.mounts}

			admitted := srv.enforceReadableRosterForCredential(context.Background(), run,
				dispatchParams{TaskMode: tc.taskMode}, policy, tc.siteCfgOK)
			if admitted == tc.wantRefused {
				t.Fatalf("admitted = %v, want refused = %v", admitted, tc.wantRefused)
			}
			if st.failed != tc.wantRefused {
				t.Fatalf("run failed = %v, want %v — a refusal must leave the run terminal with its reason", st.failed, tc.wantRefused)
			}
		})
	}
}

// TestResolveLLMInjections_RefusesBeforeResolvingAnySSOScope is the WIRING half:
// the guard has to run inside the phase that resolves the scope, above every
// credential read — not merely exist.
func TestResolveLLMInjections_RefusesBeforeResolvingAnySSOScope(t *testing.T) {
	h := newHarness(t)
	st := &mechanismGateStore{}
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunStarting}
	policy := &types.RunPolicySpec{}
	sandboxEnv := map[string]string{}

	_, ok := srv.resolveLLMInjections(context.Background(), run, dispatchParams{}, policy, sandboxEnv,
		nil, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, types.SiteConfig{}, false)
	if ok {
		t.Fatal("dispatch went ahead on an unreadable roster — the credential namespace was decided from a zero site config")
	}
	if !st.failed {
		t.Error("the refused run was not marked FAILED")
	}
	if len(sandboxEnv) != 0 {
		t.Errorf("the refused run had credential env staged anyway: %v", sandboxEnv)
	}
}

// TestEnforceConfiguredLLMMechanism_AuditsTheCredentialReason is Finding 3's
// server half: the dispatch refusal is a complete sentence with no
// machine-readable class, so the console could only render it as prose under
// ending kind `unknown`. The class is one map key on the audit row the refusal
// ALREADY writes — no column, no new action — plus the DECLARED lane, so the
// console's door binds to the run's own mechanism rather than to whatever the
// viewer's Claude Code row happens to say today.
func TestEnforceConfiguredLLMMechanism_AuditsTheCredentialReason(t *testing.T) {
	h := newHarness(t)
	st := &mechanismGateStore{}
	cfg := Config{}
	cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", Task: "ship it", State: types.RunStarting}
	sc := agentRoster(perUserAWSRow())
	policy := &types.RunPolicySpec{AllowedDomains: []string{"git.example.com"}}
	llm := srv.resolveLLMTransport(context.Background(), run, policy, map[string]string{},
		nil, false, "", "http://wardyn-proxy:3128", nil, awsSSOScope{perUser: true, owner: "member@corp.example"})

	if srv.enforceConfiguredLLMMechanism(context.Background(), run, sc, llm, nil) {
		t.Fatal("a per_user row with no captured session must refuse the dispatch")
	}

	var data map[string]any
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action != "run.create" || ev.Outcome != "failure" {
			continue
		}
		found = true
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("audit data: %v", err)
		}
	}
	if !found {
		t.Fatalf("no run.create/failure row in %d audit events", len(h.audit.events))
	}
	if data["reason"] != llmRefusalAuditReason {
		t.Errorf("reason = %v, want %q — the console grades the ending from this key", data["reason"], llmRefusalAuditReason)
	}
	if data["mechanism"] != string(types.AgentMechanismBedrockSSO) {
		t.Errorf("mechanism = %v, want the DECLARED lane %q", data["mechanism"], types.AgentMechanismBedrockSSO)
	}
	// The sentence itself is unchanged in kind: still the whole refusal, still
	// under `error`, because that is what the run's failure_hint and the CLI
	// both print.
	if s, _ := data["error"].(string); !strings.Contains(s, "does not substitute a different model provider") {
		t.Errorf("error = %q, want the refusal sentence", s)
	}
}

// TestEnforceCreateLLMMechanism_AuditsNothing: the 422 twin refuses BEFORE a run
// row exists, so there is no run to audit against and no `credential` ending for
// the console to grade — the rail renders the same sentence at the click. Pinned
// because the client's grading rule ("a run.create failure carrying this
// reason") would quietly acquire a second, run-less source if this ever emitted.
func TestEnforceCreateLLMMechanism_AuditsNothing(t *testing.T) {
	h := newHarness(t)
	st := &mechanismGateStore{sc: agentRoster(perUserAWSRow())}
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
	srv := New(cfg)
	rec := httptest.NewRecorder()

	if srv.enforceCreateLLMMechanism(context.Background(), rec, createRunRequest{Agent: "claude-code", Task: "ship it"},
		types.RunPolicySpec{}, nil, "member@corp.example", nil, true) {
		t.Fatal("a per_user row with no captured session must refuse at create")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	for _, ev := range h.audit.events {
		if ev.Action == "run.create" {
			t.Fatalf("create-time refusal audited %s/%s; it must not", ev.Action, ev.Outcome)
		}
	}
}

// TestLLMMechanismRemedy_TheDestinationIsThePersonsOwnDoor pins UX round B1: the
// refusal used to send every reader to "Settings → Model provider", which is the
// ADMIN's page — under a per_user row its AWS button is admin-only, so the one
// person who could repair their own captured session was sent to the one page
// that will not let them. Asserted THROUGH the constants; the sentences are
// DRAFT until M2 canon rules them.
func TestLLMMechanismRemedy_TheDestinationIsThePersonsOwnDoor(t *testing.T) {
	sharedRow := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourceShared,
	}

	// The dispatch/create refusal, per_user: the member's own two doors.
	perUserDead := llmMechanismRefusal(perUserAWSRow(), "", false, "")
	if !strings.Contains(perUserDead, llmMechanismRemedyPerUser) {
		t.Errorf("per_user refusal = %q, want the member's own destination %q", perUserDead, llmMechanismRemedyPerUser)
	}
	if strings.Contains(perUserDead, "Settings → Model provider") {
		t.Errorf("per_user refusal = %q, must not send a member to the admin's page", perUserDead)
	}

	// Shared: the admin's page stays, and "again" is dropped for the one arm
	// where nothing ever fired here.
	sharedNothing := llmMechanismRefusal(sharedRow, "", false, "")
	if !strings.Contains(sharedNothing, llmMechanismRemedySharedFirst) || strings.Contains(sharedNothing, "sign in again") {
		t.Errorf("shared not-configured refusal = %q, want %q with no \"again\"", sharedNothing, llmMechanismRemedySharedFirst)
	}
	sharedWrongLane := llmMechanismRefusal(sharedRow, types.AgentMechanismAnthropicAPIKey, true, "")
	if !strings.Contains(sharedWrongLane, llmMechanismRemedyShared) {
		t.Errorf("shared wrong-lane refusal = %q, want %q", sharedWrongLane, llmMechanismRemedyShared)
	}

	// The stored-identity refusal carries the same clause: the blob it names is
	// the member's own.
	sc := agentRoster(types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL: perUserPortal, SSOAccountID: "222222222222", SSORoleName: "New",
	})
	// ssoInject: only the captured-SSO lane carries a stored identity for a
	// roster pin to disagree with (bedrockBlobPinMismatch).
	b := bedrockAuth{ssoInject: true, ssoAccountID: "111111111111", ssoRoleName: "Old"}
	pin := pinContradictionRefusal(sc, b, true)
	if pin == "" {
		t.Fatal("a stored pair the roster no longer pins must refuse")
	}
	if !strings.Contains(pin, llmMechanismRemedyPerUser) {
		t.Errorf("per_user pin refusal = %q, want %q", pin, llmMechanismRemedyPerUser)
	}
	if !strings.Contains(pinContradictionRefusal(sc, b, false), llmMechanismRemedyShared) {
		t.Errorf("shared pin refusal = %q, want %q", pinContradictionRefusal(sc, b, false), llmMechanismRemedyShared)
	}

	// And the spent-renewal sentence — the one the field report quoted.
	if !strings.Contains(awsSSORefreshSpentRefusal(true), llmMechanismRemedyPerUser) {
		t.Errorf("per_user spent refusal = %q, want %q", awsSSORefreshSpentRefusal(true), llmMechanismRemedyPerUser)
	}
	if !strings.Contains(awsSSORefreshSpentRefusal(false), llmMechanismRemedyShared) {
		t.Errorf("shared spent refusal = %q, want %q", awsSSORefreshSpentRefusal(false), llmMechanismRemedyShared)
	}
}
