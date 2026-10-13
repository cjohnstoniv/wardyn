// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// composeAgentImages is the compose stack's map: a Claude Code image that
// carries the CLI. codex-cli resolves to its own published image.
var composeAgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}

// installShAgentImages is install.sh's map: Claude Code on Wardyn's agent base,
// which ships no vendor CLI.
var installShAgentImages = map[string]string{"claude-code": "ghcr.io/cjohnstoniv/agent-base:0.9.0"}

const setImage = "wardyn/agent-harnesses:local"

// harnessSetStore is the governance fake store plus the harness-set seam, the
// run image and the failure hint, so a create can be read back.
type harnessSetStore struct {
	*govEscapeStore
	putErr error
	hmu    sync.Mutex
	rows   map[uuid.UUID][]types.RunHarness
	images map[uuid.UUID]string
	hints  map[uuid.UUID]string
}

func (s *harnessSetStore) PutRunHarnesses(_ context.Context, runID uuid.UUID, hs []types.RunHarness) error {
	if s.putErr != nil {
		return s.putErr
	}
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.rows[runID] = hs
	return nil
}

func (s *harnessSetStore) ListRunHarnesses(_ context.Context, runID uuid.UUID) ([]types.RunHarness, error) {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	return s.rows[runID], nil
}

func (s *harnessSetStore) SetRunImage(_ context.Context, runID uuid.UUID, image string) error {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.images[runID] = image
	return nil
}

func (s *harnessSetStore) SetRunFailureHint(_ context.Context, runID uuid.UUID, hint string) error {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.hints[runID] = hint
	return nil
}

// onlyRun is the one run the fixture created.
func (s *harnessSetStore) onlyRun(t *testing.T) uuid.UUID {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(s.runs))
	}
	for id := range s.runs {
		return id
	}
	return uuid.Nil
}

// harnessSetFixture is govEscapeFixture over harnessSetStore, with images.
func harnessSetFixture(t *testing.T, cs *capStore, images map[string]string, putErr error) (*Server, *harnessSetStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := &harnessSetStore{govEscapeStore: newGovEscapeStore(cs), putErr: putErr,
		rows: map[uuid.UUID][]types.RunHarness{}, images: map[uuid.UUID]string{}, hints: map[uuid.UUID]string{}}
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit, cfg.Broker, cfg.Runner = audit, h.broker, &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.AgentImages = images
	return New(cfg), st, audit
}

func (f harnessSetCall) ask(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, srv, http.MethodPost, path, govSession(t, f.sub, []string{"eng"}, false), modeBody(f.body))
}

type harnessSetCall struct{ sub, body string }

func bodyTool(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	return b.Tool
}

var harnessDoors = []string{"/api/v1/runs", "/api/v1/runs/preflight", policyPreviewPath}

const (
	envNoHarness      = `"experience":"interactive",` + noRepos
	appNoHarness      = `"experience":"background","workload":{"kind":"command","command":"make test"},` + noRepos
	appWithCodex      = `"experience":"background","workload":{"kind":"command","command":"codex exec hi"},"tools":[` + codexTool + `],` + noRepos
	appWithClaude     = `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[` + claudeTool + `],` + noRepos
	envClaudePrompted = `"experience":"interactive","tools":[` + claudeTool + `],"startup":{"kind":"harness","tool":"claude-code","prompt":"fix the build"},` + noRepos
)

// TestProjectRunMode_StartupHarnessIsPrimary (T1): the legacy fields project
// the startup harness with its own provider, never tools[0]'s; a command with
// a harness projects that harness as an exec run.
func TestProjectRunMode_StartupHarnessIsPrimary(t *testing.T) {
	two := []client.IncludedTool{
		{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: "provider-a"},
		{ID: "codex-cli", Kind: client.IncludedToolHarness, ModelProvider: "provider-b"},
	}
	for _, tc := range []struct {
		name            string
		req             createRunRequest
		agent, provider string
	}{
		{"the startup harness, not the first", createRunRequest{Experience: client.ExperienceInteractive, Tools: two,
			Startup: &client.RunStartup{Kind: client.StartupHarness, Tool: "codex-cli"}}, "codex-cli", "provider-b"},
		{"nothing starts: the first harness", createRunRequest{Experience: client.ExperienceInteractive, Tools: two}, "claude-code", "provider-a"},
		{"no harness: none", createRunRequest{Experience: client.ExperienceInteractive}, "", ""},
		{"a command's first harness", createRunRequest{Experience: client.ExperienceBackground, Tools: two[1:],
			Workload: &client.RunWorkload{Kind: client.WorkloadCommand, Command: "make"}}, "codex-cli", "provider-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			projectRunMode(&req)
			if req.Agent != tc.agent || req.ModelProvider != tc.provider {
				t.Fatalf("projected agent %q provider %q, want %q %q", req.Agent, req.ModelProvider, tc.agent, tc.provider)
			}
		})
	}
}

// TestStartupPrompt_ShapeAndSeed (T2): a starting prompt rides only a harness
// startup, and becomes the started harness's boot seed.
func TestStartupPrompt_ShapeAndSeed(t *testing.T) {
	tool := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}}
	for name, s := range map[string]*client.RunStartup{
		"startup none":    {Kind: client.StartupNone, Prompt: "hi"},
		"startup command": {Kind: client.StartupCommand, Command: "make", Prompt: "hi"},
	} {
		req := createRunRequest{Experience: client.ExperienceInteractive, Tools: tool, Startup: s, NoRepositoriesOrDrives: true}
		refusal := applyRunMode(&req)
		if refusal == nil || refusal.status != http.StatusBadRequest || refusal.body.Error != startupPromptShapeMsg() {
			t.Errorf("%s with a prompt: refusal = %+v, want 400 %q", name, refusal, startupPromptShapeMsg())
		}
	}
	req := createRunRequest{Experience: client.ExperienceInteractive, Tools: tool, NoRepositoriesOrDrives: true,
		Startup: &client.RunStartup{Kind: client.StartupHarness, Tool: "claude-code", Prompt: "fix the build"}}
	if refusal := applyRunMode(&req); refusal != nil {
		t.Fatalf("a harness startup's prompt was refused: %+v", refusal.body)
	}
	if req.Task != "fix the build" || req.InteractiveStart != "agent" || interactiveBootSeed(requestIsInteractive(req), req.Task) == "" {
		t.Fatalf("projected task %q interactive_start %q: the prompt is not the boot seed", req.Task, req.InteractiveStart)
	}
	blank := createRunRequest{Experience: client.ExperienceInteractive, Tools: tool, NoRepositoriesOrDrives: true,
		Startup: &client.RunStartup{Kind: client.StartupHarness, Tool: "claude-code", Prompt: "  "}}
	if refusal := applyRunMode(&blank); refusal == nil || refusal.body.Reason != reasonRunModeInvalid {
		t.Errorf("a blank prompt: refusal = %+v, want run_mode_invalid", refusal)
	}
}

func set(ids ...string) []types.RunHarness {
	out := make([]types.RunHarness, len(ids))
	for i, id := range ids {
		out[i] = types.RunHarness{Harness: id}
	}
	return out
}

// TestHarnessSetImage (T3): the image comes from the set, never from the
// primary harness alone.
func TestHarnessSetImage(t *testing.T) {
	declared := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "claude-code+codex-cli": setImage,
		"acme-agent+claude-code+codex-cli": "wardyn/agent-all:local", "acme-agent": "ghcr.io/acme/agent:1"}
	for _, tc := range []struct {
		name   string
		set    []types.RunHarness
		images map[string]string
		custom bool
		ref    string
		kind   string
		reason string
	}{
		{"no harness: the agent base image", nil, declared, false, agentImage("base", declared), client.ImageSourceBase, ""},
		{"Your own tools alone: the agent base image", set("none"), declared, false, agentImage("base", declared), client.ImageSourceBase, ""},
		{"one harness: its own image", set("codex-cli"), declared, false, agentImage("codex-cli", declared), client.ImageSourceAgent, ""},
		{"Your own tools beside one harness: that harness's image", set("none", "codex-cli"), declared, false, agentImage("codex-cli", declared), client.ImageSourceAgent, ""},
		{"two harnesses: the declared set image", set("codex-cli", "claude-code"), declared, false, setImage, client.ImageSourceHarnessSet, ""},
		{"the smallest superset wins", set("claude-code", "codex-cli"), declared, false, setImage, client.ImageSourceHarnessSet, ""},
		{"a larger set when only it covers", set("acme-agent", "codex-cli"), declared, false, "wardyn/agent-all:local", client.ImageSourceHarnessSet, ""},
		{"one harness whose own image has no CLI: a set image that carries it", set("claude-code"), map[string]string{"claude-code+codex-cli": setImage}, false, setImage, client.ImageSourceHarnessSet, ""},
		{"one harness keeps a custom image", set("claude-code"), declared, true, "wardyn/agent-claude-code:local", client.ImageSourceAgent, ""},
		{"no declared image", set("claude-code", "codex-cli"), composeAgentImages, false, "", "", reasonHarnessSetNoImage},
		{"two harnesses and a custom image", set("claude-code", "codex-cli"), declared, true, "", "", reasonHarnessSetCustomImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact, refusal := harnessSetImage(tc.set, tc.images, tc.custom)
			if tc.reason != "" {
				if refusal == nil || refusal.body.Reason != tc.reason || refusal.body.Tool == "" {
					t.Fatalf("refusal = %+v, want %s naming a tool", refusal, tc.reason)
				}
				return
			}
			if refusal != nil || fact.Ref != tc.ref || fact.Source.Kind != tc.kind {
				t.Fatalf("image = %+v refusal %+v, want %s from %s", fact, refusal, tc.ref, tc.kind)
			}
		})
	}
	_, refusal := harnessSetImage(set("acme-agent", "claude-code", "codex-cli", "other"), declared, false)
	if want := harnessSetNoImageMsg("acme-agent", "Claude Code", "Codex CLI", "other"); refusal == nil || refusal.body.Error != want {
		t.Errorf("four harnesses: %+v, want %q", refusal, want)
	}
}

// TestHarnessInstalled (T4): installed is never guessed beyond the images.
func TestHarnessInstalled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		images map[string]string
		want   bool
	}{
		{"install.sh maps Claude Code to the agent base image", "claude-code", installShAgentImages, false},
		{"no map: the catalogue fallback is the agent base image", "claude-code", nil, false},
		{"the local agent base image", "claude-code", map[string]string{"claude-code": "wardyn/agent-base:local"}, false},
		{"compose maps Claude Code to an image with the CLI", "claude-code", composeAgentImages, true},
		{"codex-cli: Wardyn's published image", "codex-cli", nil, true},
		{"an operator's image for an agent", "acme-agent", map[string]string{"acme-agent": "ghcr.io/acme/agent:1"}, true},
		{"an unmapped custom agent", "acme-agent", nil, false},
		{"Your own tools: the agent base image", "none", nil, true},
		{"a declared set's member", "claude-code", map[string]string{"claude-code": "ghcr.io/cjohnstoniv/agent-base:0.9.0", "claude-code+codex-cli": setImage}, true},
	} {
		if got := harnessInstalled(tc.id, tc.images); got != tc.want {
			t.Errorf("%s: installed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestValidateHarnessSetImages (T5): set keys are refused at boot unless they
// are two or more valid ids, each once, sorted; a roster id cannot be one.
func TestValidateHarnessSetImages(t *testing.T) {
	if err := ValidateHarnessSetImages(map[string]string{"claude-code": "x", "claude-code+codex-cli": setImage}); err != nil {
		t.Fatalf("a valid key: %v", err)
	}
	for _, key := range []string{"claude-code+", "codex-cli+claude-code", "claude-code+claude-code", "claude-code+a b", "+codex-cli"} {
		if err := ValidateHarnessSetImages(map[string]string{key: setImage}); err == nil {
			t.Errorf("%q was accepted", key)
		}
	}
	if err := ValidateHarnessSetImages(map[string]string{"claude-code+codex-cli": " "}); err == nil {
		t.Error("a set key naming no image was accepted")
	}
	roster := &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code+codex-cli"}}}
	if err := validateAgentProviders(roster, map[string]string{"claude-code+codex-cli": setImage}); err == nil || !strings.Contains(err.Error(), "+") {
		t.Errorf("a roster id that is a set key: %v, want refused", err)
	}
}

// memberWith is a member request against a capability store.
func memberWith(t *testing.T, cs *capStore, images map[string]string) (*Server, *http.Request) {
	t.Helper()
	h := capPolicyServer(t, cs.grants, cs.enf)
	h.srv.cfg.AgentImages = images
	return h.srv, memberRequest(t)
}

func twoHarnessRequest(startup string, tools ...client.IncludedTool) createRunRequest {
	req := createRunRequest{Experience: client.ExperienceInteractive, Tools: tools, NoRepositoriesOrDrives: true}
	if startup != "" {
		req.Startup = &client.RunStartup{Kind: client.StartupHarness, Tool: startup}
	}
	projectRunMode(&req)
	return req
}

// TestHarnessSet_CapAgentEveryHarness (T8): every included harness meets the
// agent capability, not only the projected one.
func TestHarnessSet_CapAgentEveryHarness(t *testing.T) {
	cs := &capStore{enf: map[string]bool{capAgent: true},
		grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capAgent, "claude-code", types.CapabilityAllow)}}
	srv, r := memberWith(t, cs, composeAgentImages)
	req := twoHarnessRequest("claude-code",
		client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness},
		client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness})
	_, _, refusal := srv.harnessSetRefusalFor(r, req, nil)
	if refusal == nil || refusal.decision == nil || refusal.decision.Reason != authz.ReasonCapabilityAgent ||
		refusal.decision.Target != "runs.tools" || refusal.decision.Detail["tool"] != "codex-cli" {
		t.Fatalf("refusal = %+v, want capability_agent on runs.tools naming codex-cli", refusal)
	}
	cs.grants = append(cs.grants, grant(types.CapabilitySubjectAll, "", capAgent, "codex-cli", types.CapabilityAllow))
	srv, r = memberWith(t, cs, map[string]string{"claude-code+codex-cli": setImage})
	if got, _, refusal := srv.harnessSetRefusalFor(r, req, nil); refusal != nil || len(got) != 2 {
		t.Fatalf("both granted: set %+v refusal %+v", got, refusal)
	}
}

// harnessFold folds one request's model-provider step at one door.
func harnessFold(srv *Server, r *http.Request, mode foldMode, req createRunRequest) (*httptest.ResponseRecorder, runFold, bool) {
	w := httptest.NewRecorder()
	f := runFold{mode: mode, w: w, r: r, req: &req, harnesses: harnessSet(req)}
	ok := srv.stepModelProvider(&f, &foldRecorder{})
	return w, f, ok
}

// TestHarnessProviders_EachAuthorized (T6, DR-0069): the harness beside the
// primary has its own model provider judged at every door: the grant, and the
// caller's own credential where the door reads one.
func TestHarnessProviders_EachAuthorized(t *testing.T) {
	enforced := map[string]bool{capModelProvider: true}
	site := types.SiteConfig{ModelProviders: providerBlock(func() types.ModelProvider {
		p := keyProvider("openai", "codex-cli")
		p.Kind = types.ModelProviderOpenAIAPIKey
		return p
	}())}
	codex := func(provider string) createRunRequest {
		return twoHarnessRequest("",
			client.IncludedTool{ID: "none", Kind: client.IncludedToolHarness},
			client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness, ModelProvider: provider})
	}
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		cs := &capStore{enf: enforced}
		srv := providerRunFixture(t, site, cs, nil)
		r := memberRequest(t)

		// Named and not granted: answered as a provider that does not exist.
		w, _, ok := harnessFold(srv, r, mode, codex("openai"))
		if ok || w.Code != http.StatusUnprocessableEntity || bodyTool(t, w) != "codex-cli" {
			t.Fatalf("mode %d, named ungranted: ok=%v %d %s, want 422 naming codex-cli", mode, ok, w.Code, w.Body.String())
		}
		// Not named, and the one provider serving it is not granted: a 403. The
		// preview leaves an unnamed choice pending, as it does for the primary.
		w, _, ok = harnessFold(srv, r, mode, codex(""))
		if mode == foldPreview {
			if !ok {
				t.Fatalf("preview, none granted and none named: %d %s, want pending", w.Code, w.Body.String())
			}
		} else if ok || w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonCapabilityModelProvider) || bodyTool(t, w) != "codex-cli" {
			t.Fatalf("mode %d, none granted: ok=%v %d %s, want 403 capability_model_provider naming codex-cli", mode, ok, w.Code, w.Body.String())
		}

		// Granted: the preview authorizes the choice; the other doors read the
		// caller's own key for it, which this member has not added.
		cs.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capModelProvider, "openai", types.CapabilityAllow)}
		w, f, ok := harnessFold(srv, r, mode, codex("openai"))
		if mode == foldPreview {
			if !ok || f.harnesses[1].ModelProviderID != "openai" {
				t.Fatalf("preview, granted: ok=%v harnesses %+v %s", ok, f.harnesses, w.Body.String())
			}
			if fact := harnessSetFact(f.harnesses, f.harnessChoices); fact.Harnesses[1].ModelCredential == nil || fact.SharedModelAccess {
				t.Fatalf("preview fact = %+v, want codex-cli's credential and no shared access", fact)
			}
			continue
		}
		if ok || errorReason(w) != llmRefusalAuditReason || bodyTool(t, w) != "codex-cli" {
			t.Fatalf("mode %d, granted without the caller's key: ok=%v %d %s, want model_credential naming codex-cli", mode, ok, w.Code, w.Body.String())
		}
	}
}

// TestHarnessProviders_Conflict (T7): two harnesses on different providers that
// reach one host, or two Azure Foundry providers, are refused; one shared
// provider is not.
func TestHarnessProviders_Conflict(t *testing.T) {
	gateway := func(id, harness string) types.ModelProvider {
		p := keyProvider(id, harness)
		p.Kind, p.BaseURL = types.ModelProviderCustomEndpoint, "https://gw.example.com/"+id
		return p
	}
	shared := gateway("gw", "claude-code")
	shared.Harnesses = append(shared.Harnesses, types.ProviderHarness{Harness: "codex-cli"})
	tools := func(a, b string) createRunRequest {
		return twoHarnessRequest("claude-code",
			client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: a},
			client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness, ModelProvider: b})
	}
	for _, tc := range []struct {
		name      string
		providers []types.ModelProvider
		a, b      string
		conflict  bool
	}{
		{"two providers on one gateway host", []types.ModelProvider{gateway("gw-claude", "claude-code"), gateway("gw-codex", "codex-cli")}, "gw-claude", "gw-codex", true},
		{"one provider shared by both", []types.ModelProvider{shared}, "gw", "gw", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(tc.providers...)}, &capStore{}, nil)
			w, _, ok := harnessFold(srv, memberRequest(t), foldPreview, tools(tc.a, tc.b))
			if tc.conflict {
				if ok || errorReason(w) != reasonHarnessProvidersConflict || bodyTool(t, w) != "codex-cli" {
					t.Fatalf("ok=%v %d %s, want harness_providers_conflict naming codex-cli", ok, w.Code, w.Body.String())
				}
				return
			}
			if !ok {
				t.Fatalf("refused: %d %s", w.Code, w.Body.String())
			}
		})
	}
	azure := func(id, harness string) harnessChoice {
		return harnessChoice{harness: harness, choice: runProviderChoice{chosen: true, provider: types.ModelProvider{ID: id, Kind: types.ModelProviderAzureFoundry}}}
	}
	if _, b, conflict := harnessProvidersConflict([]harnessChoice{azure("az-1", "claude-code"), azure("az-2", "codex-cli")}); !conflict || b != "codex-cli" {
		t.Errorf("two Azure Foundry providers: conflict=%v %q, want codex-cli refused", conflict, b)
	}
	// A run carries one Azure Foundry lane, so even one provider can't serve two harnesses' lanes.
	if _, _, conflict := harnessProvidersConflict([]harnessChoice{azure("az-1", "claude-code"), azure("az-1", "codex-cli")}); !conflict {
		t.Error("one Azure Foundry provider on two harnesses was admitted")
	}
}

// TestGovernance_CommandHarnessUnderHold (T9, A14a): nothing pauses a harness a
// command starts, so a command with any harness is refused under a hold-rules
// profile at every door, before the codex-cli arm; without the profile it runs.
func TestGovernance_CommandHarnessUnderHold(t *testing.T) {
	const want = `A harness your command can start is not allowed under your governance profile "walled": its tool rules hold or deny, ` +
		`and nothing can pause a harness a command starts. Remove the harness, or choose an interactive environment.`
	for name, tc := range map[string]struct{ body, tool string }{
		"claude-code": {appWithClaude, "claude-code"},
		"codex-cli":   {appWithCodex, "codex-cli"},
	} {
		for _, door := range harnessDoors {
			srv, _, _ := harnessSetFixture(t, assignedStore(holdProfile("walled")), composeAgentImages, nil)
			w := harnessSetCall{"sub-walled", tc.body}.ask(t, srv, door)
			if w.Code != http.StatusForbidden || refusalBody(t, w) != want || bodyTool(t, w) != tc.tool {
				t.Errorf("%s at %s = %d %s, want 403 naming %s", name, door, w.Code, w.Body.String(), tc.tool)
			}
		}
	}
	srv, st, _ := harnessSetFixture(t, &capStore{}, composeAgentImages, nil)
	if w := (harnessSetCall{"sub-plain", appWithCodex}).ask(t, srv, harnessDoors[0]); w.Code != http.StatusCreated {
		t.Fatalf("no profile: %d %s, want 201", w.Code, w.Body.String())
	}
	srv.WaitBackground()
	if rows := st.rows[st.onlyRun(t)]; len(rows) != 1 || rows[0].Harness != "codex-cli" {
		t.Fatalf("run_harnesses = %+v, want codex-cli", rows)
	}
}

// TestEnvironmentAndApplicationShapes (T10): no harness starts from the agent
// base image, and a command may include one harness, whose image it gets.
func TestEnvironmentAndApplicationShapes(t *testing.T) {
	base := agentImage("base", composeAgentImages)
	for _, tc := range []struct {
		name, body, image string
		execMode          bool
		rows              int
	}{
		{"an environment with no harness", envNoHarness, base, false, 0},
		{"an application with no harness, agent or image", appNoHarness, base, true, 0},
		{"an application with one harness", appWithCodex, agentImage("codex-cli", nil), true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit := harnessSetFixture(t, &capStore{}, composeAgentImages, nil)
			call := harnessSetCall{"sub-plain", tc.body}
			for _, door := range harnessDoors[1:] {
				w := call.ask(t, srv, door)
				var facts struct{ Image *client.ImageFact }
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &facts) != nil || facts.Image == nil || facts.Image.Ref != tc.image {
					t.Fatalf("%s = %d %s, want 200 with image %s", door, w.Code, w.Body.String(), tc.image)
				}
			}
			if w := call.ask(t, srv, harnessDoors[0]); w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body.String())
			}
			srv.WaitBackground()
			id := st.onlyRun(t)
			if st.images[id] != tc.image || len(st.rows[id]) != tc.rows {
				t.Fatalf("image %q rows %+v, want %s and %d rows", st.images[id], st.rows[id], tc.image, tc.rows)
			}
			if got := createAuditField(t, audit, "task_mode") == "exec"; got != tc.execMode {
				t.Errorf("run.create task_mode exec = %v, want %v", got, tc.execMode)
			}
		})
	}
}

func createAuditField(t *testing.T, audit *recRecorder, key string) any {
	t.Helper()
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.create" && ev.Outcome == "success" {
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			return data[key]
		}
	}
	t.Fatal("no run.create row")
	return nil
}

// TestRunHarnesses_PersistedOrAborted (T11, abort half): the set is written
// right after the run row and audited; a write that fails aborts the run.
func TestRunHarnesses_PersistedOrAborted(t *testing.T) {
	srv, st, audit := harnessSetFixture(t, &capStore{}, composeAgentImages, nil)
	if w := (harnessSetCall{"sub-plain", envClaudePrompted}).ask(t, srv, harnessDoors[0]); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	srv.WaitBackground()
	if rows := st.rows[st.onlyRun(t)]; len(rows) != 1 || rows[0] != (types.RunHarness{Harness: "claude-code", Starts: true}) {
		t.Fatalf("run_harnesses = %+v", rows)
	}
	harnesses, _ := json.Marshal(createAuditField(t, audit, "harnesses"))
	source, _ := json.Marshal(createAuditField(t, audit, "image_source"))
	if string(harnesses) != `[{"id":"claude-code","model_provider":"","starts":true}]` || string(source) != `{"kind":"agent","name":"claude-code"}` {
		t.Errorf("run.create harnesses %s image_source %s", harnesses, source)
	}

	srv, st, _ = harnessSetFixture(t, &capStore{}, composeAgentImages, errors.New("insert refused"))
	if w := (harnessSetCall{"sub-plain", envClaudePrompted}).ask(t, srv, harnessDoors[0]); w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed write = %d %s, want 500", w.Code, w.Body.String())
	}
	id := st.onlyRun(t)
	st.mu.Lock()
	state := st.states[id]
	st.mu.Unlock()
	if state != types.RunFailed || st.hints[id] != createRunHarnessesAbortHint {
		t.Fatalf("run state %q hint %q, want FAILED with %q", state, st.hints[id], createRunHarnessesAbortHint)
	}
}

// TestHarnessShapes_PreflightAgreesWithCreate is the R2-0044 parity row for
// the new shapes: Review answers what launch answers, at all three doors.
func TestHarnessShapes_PreflightAgreesWithCreate(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		images     map[string]string
		reason     string
		status     int
	}{
		{"no harness", envNoHarness, composeAgentImages, "", 0},
		{"a command with no harness", appNoHarness, composeAgentImages, "", 0},
		{"a command with a harness", appWithCodex, composeAgentImages, "", 0},
		{"a starting prompt", envClaudePrompted, composeAgentImages, "", 0},
		{"a harness not installed", `"experience":"interactive","tools":[` + claudeTool + `],` + noRepos, installShAgentImages, reasonHarnessNotInstalled, http.StatusUnprocessableEntity},
		{"a prompt with no harness startup", envNoHarness + `,"startup":{"kind":"none","prompt":"hi"}`, composeAgentImages, reasonRunModeInvalid, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, door := range harnessDoors {
				srv, _, _ := harnessSetFixture(t, &capStore{}, tc.images, nil)
				w := harnessSetCall{"sub-plain", tc.body}.ask(t, srv, door)
				if tc.status == 0 {
					if w.Code >= 300 {
						t.Errorf("%s = %d %s, want success", door, w.Code, w.Body.String())
					}
					continue
				}
				if w.Code != tc.status || errorReason(w) != tc.reason {
					t.Errorf("%s = %d %s, want %d %s", door, w.Code, w.Body.String(), tc.status, tc.reason)
				}
				if tc.reason == reasonHarnessNotInstalled && bodyTool(t, w) != "claude-code" {
					t.Errorf("%s names tool %q, want claude-code", door, bodyTool(t, w))
				}
			}
		})
	}
}

// TestSetupStatus_HarnessFacts: installed, approval_hold and the declared
// combinations reach setup status, narrowed to the harnesses listed.
func TestSetupStatus_HarnessFacts(t *testing.T) {
	images := map[string]string{"claude-code": "ghcr.io/cjohnstoniv/agent-base:0.9.0", "claude-code+codex-cli": setImage, "acme+claude-code": "x"}
	tools := setupHarnessTools(types.SiteConfig{}, images, nil)
	got := map[string][2]bool{}
	for _, h := range tools {
		got[h.ID] = [2]bool{h.Installed, h.ApprovalHold}
	}
	want := map[string][2]bool{"claude-code": {true, true}, "codex-cli": {true, false}, "none": {true, false}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: installed, approval_hold = %v, want %v", id, got[id], w)
		}
	}
	sets := visibleHarnessSets(images, tools)
	if len(sets) != 1 || strings.Join(sets[0], "+") != "claude-code+codex-cli" {
		t.Errorf("harness_sets = %v, want only claude-code+codex-cli (acme is not listed)", sets)
	}
	if sets := visibleHarnessSets(nil, tools); sets == nil || len(sets) != 0 {
		t.Errorf("no declared set: %v, want [] (never null)", sets)
	}
}

// TestHarnessProviderPairsConflict (fix-r1 M2): the one predicate create and
// dispatch share. A host both harnesses reach conflicts unless they would
// author the same grant; a second Azure Foundry lane always does.
func TestHarnessProviderPairsConflict(t *testing.T) {
	endpoint := func(id string, harnesses ...types.ProviderHarness) types.ModelProvider {
		return types.ModelProvider{ID: id, Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://gw.example.com", Harnesses: harnesses}
	}
	claude, codex := types.ProviderHarness{Harness: "claude-code"}, types.ProviderHarness{Harness: "codex-cli"}
	codexBearer := types.ProviderHarness{Harness: "codex-cli", AuthHeader: "x-codex-key", AuthFormat: "%s"}
	azure := func(id string) types.ModelProvider {
		return types.ModelProvider{ID: id, Kind: types.ModelProviderAzureFoundry, Harnesses: []types.ProviderHarness{claude, codex}}
	}
	for _, tc := range []struct {
		name     string
		pa, pb   types.ModelProvider
		conflict bool
	}{
		{"one endpoint, one grant: the same key sent the same way", endpoint("gw", claude, codex), endpoint("gw", claude, codex), false},
		{"one endpoint whose header differs per harness", endpoint("gw", claude, codexBearer), endpoint("gw", claude, codexBearer), true},
		{"two providers on one host", endpoint("gw-a", claude), endpoint("gw-b", codex), true},
		{"two providers on different hosts", endpoint("gw-a", claude), func() types.ModelProvider {
			p := endpoint("gw-b", codex)
			p.BaseURL = "https://other.example.com"
			return p
		}(), false},
		{"a second Azure Foundry provider", azure("az-1"), azure("az-2"), true},
		{"a Claude subscription through the endpoint's host", types.ModelProvider{ID: "sub", Kind: types.ModelProviderAnthropicSubscription,
			BaseURL: "https://gw.example.com", Harnesses: []types.ProviderHarness{claude}}, endpoint("gw-b", codex), true},
		{"a gateway-routed subscription beside an endpoint on the vendor host", types.ModelProvider{ID: "sub", Kind: types.ModelProviderAnthropicSubscription,
			BaseURL: "https://gw.example.com", Harnesses: []types.ProviderHarness{claude}}, func() types.ModelProvider {
			p := endpoint("gw-b", codex)
			p.BaseURL = "https://api.anthropic.com"
			return p
		}(), true},
		{"Bedrock beside an endpoint on its runtime host", types.ModelProvider{ID: "br", Kind: types.ModelProviderBedrockBearer,
			Bedrock: &types.BedrockSettings{Region: "us-west-2", BaseURL: "https://gw.example.com"}, Harnesses: []types.ProviderHarness{claude}}, endpoint("gw-b", codex), true},
	} {
		if got := harnessProviderPairsConflict("claude-code", tc.pa, "codex-cli", tc.pb); got != tc.conflict {
			t.Errorf("%s: conflict = %v, want %v", tc.name, got, tc.conflict)
		}
	}
}

// TestHarnessSet_CustomImageSkipsTheInstalledGate (fix-r1 L1): a run that
// names its own image is not known to lack the CLI.
func TestHarnessSet_CustomImageSkipsTheInstalledGate(t *testing.T) {
	srv, r := memberWith(t, &capStore{}, installShAgentImages)
	req := twoHarnessRequest("claude-code", client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness})
	if _, _, refusal := srv.harnessSetRefusalFor(r, req, nil); refusal == nil || refusal.body.Reason != reasonHarnessNotInstalled {
		t.Fatalf("no image of its own: %+v, want harness_not_installed", refusal)
	}
	req.Image = "ghcr.io/acme/claude:1"
	set, image, refusal := srv.harnessSetRefusalFor(r, req, nil)
	if refusal != nil || len(set) != 1 || image != nil {
		t.Fatalf("its own image: set %+v image %+v refusal %+v, want admitted with no image fact", set, image, refusal)
	}
}

// TestHarnessSetKeyIsNotAnAgent (fix-r1 L3): a set key named as an older
// client's agent is refused at every door, roster or not.
func TestHarnessSetKeyIsNotAnAgent(t *testing.T) {
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "claude-code+codex-cli": setImage}
	for _, door := range harnessDoors {
		srv, _, _ := harnessSetFixture(t, &capStore{}, images, nil)
		w := doSSO(t, srv, http.MethodPost, door, govSession(t, "sub-plain", []string{"eng"}, false), `{"agent":"claude-code+codex-cli","task":"t"}`)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonAgentNotEnabled {
			t.Errorf("%s = %d %s, want 422 agent_not_enabled", door, w.Code, w.Body.String())
		}
	}
}
