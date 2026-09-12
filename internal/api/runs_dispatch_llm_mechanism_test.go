// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
				c.injections, c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil)

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
				c.injections, c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil)

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
				nil, false, c.taskMode, "http://wardyn-proxy:3128", nil)

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
// C4 INVERTS THE LAST ROW: resolveBedrockAuth still reads the OPERATOR's blob,
// so a per_user row whose operator blob resolves is admitted TODAY. When C4
// gives that resolver its perUser flag (own blob only, no fall-through), the
// member's case becomes "nothing selected" and this same predicate refuses it —
// flip the row's want and delete this paragraph.
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

// TestLLMMechanismRefusal_NamesTheDeclaredLaneAndItsState: the refusal says what
// was configured, that it is not working, and that nothing is substituted for it.
// A renewable captured SSO session that could not be renewed carries C1's own
// sentence instead, which names WHY.
func TestLLMMechanismRefusal_NamesTheDeclaredLaneAndItsState(t *testing.T) {
	row := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO}
	msg := llmMechanismRefusal(row, "")
	for _, want := range []string{
		llmMechanismWords[types.AgentMechanismBedrockSSO],
		llmMechanismStateNotConfigured,
		"Wardyn does not substitute a different model provider.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q is missing %q", msg, want)
		}
	}
	if got := llmMechanismRefusal(row, awsSSORefreshSpentSentence); got != awsSSORefreshSpentSentence {
		t.Errorf("a renewal failure must carry its own sentence, got %q", got)
	}
	// Another declared lane's refusal must not borrow the SSO sentence.
	apiKey := types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey}
	if got := llmMechanismRefusal(apiKey, awsSSORefreshSpentSentence); got == awsSSORefreshSpentSentence {
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

			ok := srv.enforceCreateLLMMechanism(context.Background(), rec, c.req, types.RunPolicySpec{}, nil)
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
		})
	}
}

// TestCreateRunAuditData_CarriesClampWarnings: the run-bound run.create success
// datum is where the tightening list lives (the run-detail "Effective policy"
// widget's only join key), and it is absent — not empty — when nothing narrowed.
func TestCreateRunAuditData_CarriesClampWarnings(t *testing.T) {
	warns := []string{"resources capped to operator maximum", `dropped 1 egress domain(s) not in operator allowlist: ["evil.example"]`}
	data := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), "jti", warns)
	got, ok := data["clamp_warnings"].([]string)
	if !ok || len(got) != len(warns) || got[0] != warns[0] {
		t.Fatalf("clamp_warnings = %#v, want %#v", data["clamp_warnings"], warns)
	}
	if _, present := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), "jti", nil)["clamp_warnings"]; present {
		t.Error("clamp_warnings must be absent when launch narrowed nothing")
	}
}
