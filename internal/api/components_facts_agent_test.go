// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func decodedAgentFact(t *testing.T, w *httptest.ResponseRecorder) client.ComponentFact {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("door = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Components []client.ComponentFact `json:"components"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var found []client.ComponentFact
	for _, fact := range body.Components {
		if fact.Kind == types.ComponentAgent {
			found = append(found, fact)
		}
		if fact.Kind == "model_provider" {
			t.Fatal("model provider must be nested in the agent fact")
		}
	}
	if len(found) != 1 || found[0].Agent == nil {
		t.Fatalf("want exactly one agent fact: %s", w.Body.String())
	}
	if found[0].Requirements == nil || found[0].Agent.Hosts == nil || found[0].Agent.Secrets == nil {
		t.Fatalf("required arrays must not be null: %+v", found[0])
	}
	return found[0]
}

func TestComponentFacts_AgentActualDoors(t *testing.T) {
	t.Setenv(envAllowAgentTelemetry, "")
	for _, agent := range []string{"claude-code", "codex-cli", "none"} {
		for _, door := range dryDoors {
			t.Run(agent+door, func(t *testing.T) {
				f := newComponentFixture(t)
				approvals := "auto"
				if agent == "claude-code" {
					approvals = "hold"
				}
				w := f.ask(t, door, map[string]any{"agent": agent, "task": "facts", "tool_approvals": approvals})
				fact := decodedAgentFact(t, w)
				componentFactGolden(t, "agent_"+agent, fact)
				if fact.Agent.Agent != agent || fact.Agent.ModelProvider != nil || len(fact.Agent.Hosts) != 0 || len(fact.Agent.Secrets) != 0 {
					t.Fatalf("no chosen model lane must not fabricate one: %+v", fact)
				}
				wantStatus := componentUnknown
				if agent == "none" {
					wantStatus = componentReady
				}
				if fact.Status != wantStatus {
					t.Fatalf("status = %q, want %q", fact.Status, wantStatus)
				}
				if agent == "claude-code" {
					path, content, _ := agentpolicy.ForAgent(agent, "", true, false)
					if fact.Agent.ManagedSettings == nil || fact.Agent.ManagedSettings.Path != path || fact.Agent.ManagedSettings.Document != string(content) {
						t.Fatalf("managed document differs from dispatch's generator: %+v", fact.Agent.ManagedSettings)
					}
					if fact.Agent.ManagedSettings.Harness != agent {
						t.Fatalf("managed settings name harness %q, want the document's own %q", fact.Agent.ManagedSettings.Harness, agent)
					}
				} else if fact.Agent.ManagedSettings != nil {
					t.Fatal("non-Claude harness has no managed-settings parser")
				}
				if fact.Agent.Telemetry == nil || !fact.Agent.Telemetry.Off || !slices.Equal(fact.Agent.Telemetry.Env, []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_TELEMETRY"}) {
					t.Fatalf("telemetry differs from dispatch: %+v", fact.Agent.Telemetry)
				}
			})
		}
	}
}

func TestComponentFacts_ModelProviderDoorIsolation(t *testing.T) {
	for _, tc := range []struct {
		agent      string
		kind       types.ModelProviderKind
		secretKind string
	}{
		{"claude-code", types.ModelProviderAnthropicAPIKey, "key"},
		{"codex-cli", types.ModelProviderOpenAIAPIKey, "key"},
		{"claude-code", types.ModelProviderCustomEndpoint, "token"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			p := keyProvider("private-row-id", tc.agent)
			p.UID, p.Name, p.Kind, p.BaseURL = "private-row-uid", "Approved provider", tc.kind, "https://model.example/v1"
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
			body := `{"agent":"` + tc.agent + `","task":"facts"}`
			member := govSession(t, govMemberSub, []string{"eng"}, false)
			preview := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body))
			if preview.Status != componentUnknown {
				t.Fatal("preview must not grade unread own credential")
			}
			missing := doSSO(t, srv, http.MethodPost, componentDoors[1], member, body)
			if missing.Code != http.StatusUnprocessableEntity || strings.Contains(missing.Body.String(), `"components"`) {
				t.Fatalf("missing credential = %d %s", missing.Code, missing.Body.String())
			}
			if err := srv.cfg.Secrets.For(govMemberSub).Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("private-own-value")); err != nil {
				t.Fatal(err)
			}
			ready := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, componentDoors[1], member, body))
			if ready.Status != componentReady || ready.Agent.ModelProvider.Name != p.Name || ready.Agent.ModelProvider.Kind != string(p.Kind) ||
				!slices.Equal(ready.Agent.Hosts, []client.AgentHostFact{{Host: "model.example", Role: "provider"}}) ||
				!slices.Equal(ready.Agent.Secrets, []client.AgentSecretFact{{Kind: tc.secretKind, Owner: "own", Residency: "proxy"}}) {
				t.Fatalf("chosen lane = %+v", ready)
			}
			wire, err := json.Marshal(ready)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{p.ID, p.UID, "private-own-value", providerSecretName(p.UID, providerKeyPart)} {
				if strings.Contains(string(wire), private) {
					t.Fatalf("fact leaks %q: %s", private, wire)
				}
			}
		})
	}
}

func TestComponentFacts_ActualSDKPreflightDecode(t *testing.T) {
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
	token := providerAdminToken(srv, "facts-operator")
	httpServer := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(httpServer.Close)
	result, err := client.New(httpServer.URL, token).Preflight(context.Background(), client.CreateRunRequest{Agent: "claude-code", Task: "facts", ToolApprovals: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Components) != 1 || result.Components[0].Agent == nil || result.Components[0].Agent.ManagedSettings == nil || result.Components[0].Agent.Hosts == nil || result.Components[0].Agent.Secrets == nil {
		t.Fatalf("SDK failed to decode actual response: %+v", result.Components)
	}
}

// An environment run names no harness; step 5 must not be told about one with an empty name.
func TestComponentFacts_NoHarnessNoAgentFact(t *testing.T) {
	for _, req := range []createRunRequest{{}, {Shape: client.RunShapeEnvironment}} {
		if fact, ok := agentComponentFact(componentFactInputs{req: &req}); ok {
			t.Fatalf("a run with no harness got an agent fact: %+v", fact)
		}
	}
	if _, ok := agentComponentFact(componentFactInputs{req: &createRunRequest{Agent: "claude-code"}}); !ok {
		t.Fatal("a run that names a harness lost its agent fact")
	}
}

func TestComponentFacts_ManagedPolicyResolutionAndTelemetry(t *testing.T) {
	for _, locked := range []bool{false, true} {
		req := createRunRequest{Agent: "claude-code", Task: "facts"}
		fold := componentFactInputs{credentialChecked: true, autonomyResolved: true, req: &req, autonomy: types.AutonomyResolution{Level: types.AutonomyL2}, ceiling: governanceCeiling{Unit: &ResolvedUnit{Name: "Approved leaf"}, Limits: types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{AgentGuardrailLocks: locked}}}}
		fact, ok := agentComponentFact(fold)
		path, content, _ := agentpolicy.ForAgent(req.Agent, fold.autonomy.Level, false, locked)
		if !ok || fact.Agent.ManagedSettings == nil || fact.Agent.ManagedSettings.Path != path || fact.Agent.ManagedSettings.Document != string(content) || fact.Agent.ManagedSettings.Locked != locked {
			t.Fatalf("frozen L2 policy differs: %+v", fact)
		}
		if locked && (fact.Agent.ManagedSettings.LockedBy != "unit" || fact.Agent.ManagedSettings.Unit != "Approved leaf") {
			t.Fatal("lock source must name the admitted leaf only")
		}
		fold.credentialChecked, fold.autonomyResolved = false, false
		preview, _ := agentComponentFact(fold)
		if preview.Agent.ManagedSettings != nil {
			t.Fatal("preview must not publish an unresolved autonomy document")
		}
	}
	t.Setenv(envAllowAgentTelemetry, "true")
	req := createRunRequest{Agent: "claude-code"}
	fact, _ := agentComponentFact(componentFactInputs{req: &req})
	if fact.Agent.Telemetry.Off || len(fact.Agent.Telemetry.Env) != 0 || len(agentTelemetryEnv()) != 0 {
		t.Fatal("operator telemetry opt-in must agree with dispatch")
	}
}

func TestComponentFacts_ActualPreflightDispatchManagedDocument(t *testing.T) {
	for _, locks := range []bool{false, true} {
		profile := autonomyProfile(types.AutonomyL2)
		profile.Name, profile.Limits.AutonomyRubric.AgentGuardrailLocks = "Approved leaf", locks
		srv, _, _ := govEscapeFixture(t, autonomyCapStore(profile))
		member := govSession(t, govMemberSub, []string{"eng"}, false)
		body := `{"agent":"claude-code","task":"facts","confinement_class":"CC2"}`
		preview := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body))
		if preview.Agent.ManagedSettings != nil {
			t.Fatal("preview did not resolve autonomy")
		}
		preflight := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, componentDoors[1], member, body))
		ceiling := governanceCeiling{Unit: &ResolvedUnit{Name: profile.Name}, Limits: profile.Limits}
		dispatched, _, _ := agentPolicyDispatchCeiling(t, &fakeRunner{}, "claude-code", types.AutonomyL2, "", dispatchParams{}, ceiling)
		if len(dispatched.ManagedFiles) != 1 || preflight.Agent.ManagedSettings == nil || preflight.Agent.ManagedSettings.Path != dispatched.ManagedFiles[0].Path || preflight.Agent.ManagedSettings.Document != string(dispatched.ManagedFiles[0].Content) || preflight.Agent.ManagedSettings.Locked != locks {
			t.Fatalf("actual preflight/dispatch policy mismatch: %+v vs %+v", preflight.Agent.ManagedSettings, dispatched.ManagedFiles)
		}
	}
}

func TestComponentFacts_CapturedProviderAndRenewableLanes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		provider        types.ModelProvider
		part            string
		blob            []byte
		kind, residency string
	}{
		{"subscription", subProvider("private-sub-row"), providerOAuthPart, subBlob("private-sub-token"), "subscription", "proxy"},
		{"bedrock bearer", brBearerProvider(), providerKeyPart, []byte("private-bearer-key"), "key", "proxy"},
		{"bedrock SSO", brSSOProvider(), providerSSOPart, brBlob("private-sso-token", "123456789012", "BedrockUser", time.Now().Add(time.Hour)), "aws", "sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.provider.Name = "Approved provider"
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(tc.provider)}, &capStore{}, nil)
			srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
			member := govSession(t, govMemberSub, []string{"eng"}, false)
			body := `{"agent":"claude-code","task":"facts"}`
			preview := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body))
			if preview.Status != componentUnknown {
				t.Fatal("preview did not read credential state")
			}
			if err := srv.cfg.Secrets.For(govMemberSub).Put(context.Background(), providerSecretName(tc.provider.UID, tc.part), tc.blob); err != nil {
				t.Fatal(err)
			}
			preflight := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, componentDoors[1], member, body))
			if preflight.Status != componentReady || !slices.Equal(preflight.Agent.Secrets, []client.AgentSecretFact{{Kind: tc.kind, Owner: "own", Residency: tc.residency}}) {
				t.Fatalf("captured lane = %+v", preflight)
			}
			if tc.provider.Kind == types.ModelProviderBedrockSSO {
				if slices.ContainsFunc(preview.Agent.Hosts, func(h client.AgentHostFact) bool { return h.Role == "login" }) {
					t.Fatal("preview guessed unread session region")
				}
				if !slices.Contains(preflight.Agent.Hosts, client.AgentHostFact{Host: ssoPortalHost("us-east-1", ""), Role: "login"}) {
					t.Fatal("preflight lost already-resolved own login host")
				}
				var blob awsSSOBlob
				if err := json.Unmarshal(tc.blob, &blob); err != nil {
					t.Fatal(err)
				}
				blob.ExpiresAt, blob.RegistrationExpiresAt = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
				blob.RefreshToken, blob.ClientID, blob.ClientSecret = "private-refresh", "private-client", "private-client-secret"
				wire, err := json.Marshal(blob)
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.cfg.Secrets.For(govMemberSub).Put(context.Background(), providerSecretName(tc.provider.UID, tc.part), wire); err != nil {
					t.Fatal(err)
				}
				w := doSSO(t, srv, http.MethodPost, componentDoors[1], member, body)
				if decodedAgentFact(t, w).Status != componentReady || !strings.Contains(w.Body.String(), "renewed at launch") {
					t.Fatalf("dry renewable lane = %s", w.Body.String())
				}
				blob.RefreshToken = ""
				wire, err = json.Marshal(blob)
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.cfg.Secrets.For(govMemberSub).Put(context.Background(), providerSecretName(tc.provider.UID, tc.part), wire); err != nil {
					t.Fatal(err)
				}
				w = doSSO(t, srv, http.MethodPost, componentDoors[1], member, body)
				if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), `"components"`) {
					t.Fatalf("expired nonrenewable lane must refuse without facts: %d %s", w.Code, w.Body.String())
				}
			}
			wire, err := json.Marshal(preflight)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{tc.provider.ID, tc.provider.UID, "private-", brStartURL, "123456789012", "BedrockUser"} {
				if strings.Contains(string(wire), private) {
					t.Fatalf("captured fact leaks %q: %s", private, wire)
				}
			}
		})
	}
}

// A non-interactive run under a profile whose tool rules hold or deny is
// derived to tool_approvals=hold at create, and dispatch writes the document
// for that value. The managed-settings fact must report the same document.
func TestComponentFacts_GovernanceHoldMatchesDispatch(t *testing.T) {
	const body = `{"agent":"claude-code","task":"facts","confinement_class":"CC2"}`
	for _, tc := range []struct {
		name  string
		level types.AutonomyLevel // "" = no autonomy rubric
		rules []types.ToolRule
		hold  bool // create derives tool_approvals=hold
	}{
		{"no rubric, deny rule", "", []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}, true},
		{"no rubric, hold rule", "", []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, true},
		{"rubric L2, deny rule", types.AutonomyL2, []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}, true},
		{"no rubric, no rules (characterization)", "", nil, false},
		{"rubric L2, no rules (characterization)", types.AutonomyL2, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := govProfile("hold-facts")
			profile.Limits = types.GovernanceLimits{}
			if tc.level != "" {
				profile.Limits = autonomyProfile(tc.level).Limits
			}
			profile.Ceiling.ToolRules = tc.rules
			srv, _, _ := govEscapeFixture(t, autonomyCapStore(profile))
			member := govSession(t, govMemberSub, []string{"eng"}, false)
			ceiling := governanceCeiling{Unit: &ResolvedUnit{Name: profile.Name}, Limits: profile.Limits, Spec: profile.Ceiling}
			// The value create dispatches with, from create's own derivation rather than set by hand.
			params := dispatchParams{ToolApprovals: effectiveToolApprovals(createRunRequest{Agent: "claude-code", Task: "facts"}, ceiling)}
			if (params.ToolApprovals == "hold") != tc.hold {
				t.Fatalf("create derives tool_approvals %q, want hold=%v", params.ToolApprovals, tc.hold)
			}
			dispatched, _, _ := agentPolicyDispatchCeiling(t, &fakeRunner{}, "claude-code", tc.level, "facts", params, ceiling)
			doors := []string{componentDoors[1]}
			if tc.level == "" {
				doors = append(doors, componentDoors[2]) // the preview resolves only an unbound run
			}
			for _, door := range doors {
				fact := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, door, member, body)).Agent.ManagedSettings
				if len(dispatched.ManagedFiles) == 0 {
					if fact != nil {
						t.Fatalf("%s reports %+v; dispatch delivers no managed settings", door, fact)
					}
					continue
				}
				if len(dispatched.ManagedFiles) != 1 || fact == nil || fact.Path != dispatched.ManagedFiles[0].Path || fact.Document != string(dispatched.ManagedFiles[0].Content) {
					t.Fatalf("%s reports %+v; dispatch delivers %d file(s), first: %s", door, fact, len(dispatched.ManagedFiles), dispatched.ManagedFiles[0].Content)
				}
			}
		})
	}
}

// managedSettingsFactOf is the managed-settings fact one dry door publishes.
func managedSettingsFactOf(t *testing.T, w *httptest.ResponseRecorder) *client.ManagedSettingsFact {
	t.Helper()
	return decodedAgentFact(t, w).Agent.ManagedSettings
}

// DR-0196 (a): an interactive Claude Code run's policy tool rules are written
// into its managed settings by dispatch (attendedToolRules), so Review's fact
// must carry them, not the bare level document.
func TestComponentFacts_InteractiveAttendedRulesMatchDispatch(t *testing.T) {
	profile := autonomyProfile(types.AutonomyL2)
	profile.Ceiling.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}
	srv, _, _ := govEscapeFixture(t, autonomyCapStore(profile))
	member := govSession(t, govMemberSub, []string{"eng"}, false)
	const body = `{"agent":"claude-code","interactive":true,"confinement_class":"CC2"}`
	req := createRunRequest{Agent: "claude-code", Interactive: true}
	want, err := agentPolicyDocument("claude-code", types.AutonomyL2, nil, holdLane(true, "", ""), false, profile.Ceiling.ToolRules)
	if err != nil || !want.ok || want.toolRules == 0 {
		t.Fatalf("dispatch's document for these inputs = %+v, %v; want one carrying the attended rules", want, err)
	}
	if _, bare, _ := agentpolicy.ForAgent(req.Agent, types.AutonomyL2, holdLane(true, "", ""), false); string(bare) == string(want.content) {
		t.Fatal("the attended rules changed nothing; the test proves nothing")
	}
	got := managedSettingsFactOf(t, doSSO(t, srv, http.MethodPost, componentDoors[1], member, body))
	if got == nil || got.Path != want.path || got.Document != string(want.content) {
		t.Fatalf("preflight reports %+v; dispatch writes %s at %s", got, want.content, want.path)
	}
}

// DR-0196 (b): the started agent has no managed-settings document but a
// harness in the run's set does; dispatch writes that harness's document, so
// Review's fact is present and is that document.
func TestComponentFacts_HarnessSetFallbackMatchesDispatch(t *testing.T) {
	srv, _, _ := harnessSetFixture(t, autonomyCapStore(autonomyProfile(types.AutonomyL2)), map[string]string{"claude-code+codex-cli": setImage}, nil)
	body := `"experience":"interactive","tools":[` + claudeTool + `,` + codexTool + `],` +
		`"startup":{"kind":"harness","tool":"codex-cli","prompt":"fix the build"},` + noRepos
	set := []types.RunHarness{{Harness: "claude-code"}, {Harness: "codex-cli", Starts: true}}
	want, err := agentPolicyDocument("codex-cli", types.AutonomyL2, set, holdLane(true, "", ""), false, nil)
	if err != nil || !want.ok || want.harness != "claude-code" {
		t.Fatalf("dispatch's document for these inputs = %+v, %v; want claude-code's", want, err)
	}
	got := managedSettingsFactOf(t, harnessSetCall{govMemberSub, body}.ask(t, srv, harnessDoors[1]))
	if got == nil || got.Path != want.path || got.Document != string(want.content) {
		t.Fatalf("preflight reports %+v; dispatch writes %s at %s", got, want.content, want.path)
	}
	if got.Harness != want.harness {
		t.Errorf("managed_settings.harness = %q, want %q: the fact names the harness whose document dispatch writes, not the started codex-cli", got.Harness, want.harness)
	}
}

// Control: an exec run has no agent process, so no agent fact, as before.
func TestComponentFacts_ExecRunUnchanged(t *testing.T) {
	f := newComponentFixture(t)
	for _, door := range dryDoors {
		w := f.ask(t, door, map[string]any{"agent": "claude-code", "task": "make", "task_mode": "exec"})
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"managed_settings"`) || strings.Contains(w.Body.String(), `"kind":"agent"`) {
			t.Fatalf("%s exec = %d %s", door, w.Code, w.Body.String())
		}
	}
}

// FUG-L748: the harness the fact names is the one whose document dispatch writes, which is not the
// started agent's when it has none: a codex-cli start in a set with claude-code carries claude-code's.
func TestAgentPolicyDocumentNamesTheHarnessWhoseDocumentItIs(t *testing.T) {
	set := []types.RunHarness{{Harness: "codex-cli"}, {Harness: claudeCodeHarness}}
	doc, err := agentPolicyDocument("codex-cli", "", set, true, false, nil)
	if err != nil || !doc.ok {
		t.Fatalf("document = %+v, %v", doc, err)
	}
	if doc.harness != claudeCodeHarness {
		t.Errorf("document harness = %q, want %q: codex-cli has none, so the set's first that does supplies it", doc.harness, claudeCodeHarness)
	}
}
