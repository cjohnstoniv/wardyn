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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	codexOwnTask = `{"id":"codex-cli","kind":"harness","task":"write the tests"}`
	twoAgents    = `"experience":"background",` + agentTask + `,"tools":[` + claudeTool + `,` + codexOwnTask + `],` + noRepos
)

// multiAgentReq is a background run of workload.agent on its task, with tools in order.
func multiAgentReq(t *testing.T, agent string, tools ...client.IncludedTool) createRunRequest {
	t.Helper()
	req := createRunRequest{
		Experience: client.ExperienceBackground, NoRepositoriesOrDrives: true, Tools: tools,
		Workload: &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: agent, Task: "the run's task"},
	}
	if f := applyRunMode(&req); f != nil {
		t.Fatalf("applyRunMode: %d %s %s", f.status, f.body.Reason, f.body.Error)
	}
	return req
}

func harnessTool(id, task string, rules ...types.ToolRule) client.IncludedTool {
	return client.IncludedTool{ID: id, Kind: client.IncludedToolHarness, Task: task, ToolRules: rules}
}

func ruleFor(rules []types.ToolRule, tool string) types.ToolEffect {
	if i := slices.IndexFunc(rules, func(r types.ToolRule) bool { return r.Tool == tool }); i >= 0 {
		return rules[i].Effect
	}
	return ""
}

// TestMultiAgent_A8MeetNarrowsAToolRuleNeverWidens: a second agent's stricter rule
// binds the first agent too, and a second agent's looser rule never loosens the first.
func TestMultiAgent_A8MeetNarrowsAToolRuleNeverWidens(t *testing.T) {
	const tool = "mcp__gh__create_issue"
	policy := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}}
	allow, deny := types.ToolRule{Tool: tool, Effect: types.ToolAllow}, types.ToolRule{Tool: tool, Effect: types.ToolDeny}

	alone := multiAgentReq(t, "claude-code", harnessTool("claude-code", "", allow))
	spec, sets := applyHarnessToolRules(policy, alone.Tools, toolLaneDefault(alone))
	if len(sets) != 0 || ruleFor(spec.ToolRules, tool) != types.ToolAllow {
		t.Fatalf("claude-code alone: rules %v sets %v, want its own allow run-wide", spec.ToolRules, sets)
	}

	for name, tools := range map[string][]client.IncludedTool{
		"a stricter second agent narrows the first":    {harnessTool("claude-code", "", allow), harnessTool("codex-cli", "write the tests", deny)},
		"a looser second agent never widens the first": {harnessTool("claude-code", "", deny), harnessTool("codex-cli", "write the tests", allow)},
	} {
		req := multiAgentReq(t, "claude-code", tools...)
		_, sets := applyHarnessToolRules(policy, req.Tools, toolLaneDefault(req))
		if len(sets) != 2 {
			t.Fatalf("%s: %d harness sets, want 2", name, len(sets))
		}
		for _, set := range sets {
			if got := ruleFor(set.Rules, tool); got != types.ToolDeny {
				t.Errorf("%s: %s's %s = %q, want deny: every agent's rules met, strictest wins", name, set.Harness, tool, got)
			}
		}
	}
}

// TestMultiAgent_A8MeetNarrowsAnInnerWallNeverWidens: an agent beside codex-cli whose own
// rules need Wardyn's approvals puts the whole run on the gated lane, so Claude Code there
// launches with approvals in Wardyn, as it would alone with those rules; adding an agent
// with no rules never takes a gated run off the lane.
func TestMultiAgent_A8MeetNarrowsAnInnerWallNeverWidens(t *testing.T) {
	deny := types.ToolRule{Tool: "mcp__gh__create_issue", Effect: types.ToolDeny}
	walls := func(req createRunRequest) types.HarnessInnerWalls {
		var spec types.RunPolicySpec
		(&Server{}).applyToolPosture(&req, &spec)
		w, _ := agentpolicy.InnerWalls("claude-code", false, holdLane(false, req.ToolApprovals, req.TaskMode), false)
		return w
	}
	alone := walls(multiAgentReq(t, "claude-code", harnessTool("claude-code", "")))
	if alone.Approvals != types.InnerApprovalsNone {
		t.Fatalf("claude-code alone with no rules: approvals %q, want none", alone.Approvals)
	}
	narrowed := walls(multiAgentReq(t, "codex-cli", harnessTool("codex-cli", ""), harnessTool("claude-code", "write the change", deny)))
	if narrowed.Approvals != types.InnerApprovalsWardyn {
		t.Errorf("claude-code beside codex-cli with its own rule: approvals %q, want wardyn (the run is on the gated lane)", narrowed.Approvals)
	}
	held := walls(multiAgentReq(t, "claude-code", harnessTool("claude-code", "", deny), harnessTool("codex-cli", "write the tests")))
	if held.Approvals != types.InnerApprovalsWardyn {
		t.Errorf("a gated claude-code beside an agent with no rules: approvals %q, want wardyn", held.Approvals)
	}
}

// TestMultiAgent_EveryAgentMustEnforceTheMetRules: a hold one agent brings binds codex-cli
// too, which cannot pause a call, so the run is refused as codex-cli's own run with that
// hold would be, naming it.
func TestMultiAgent_EveryAgentMustEnforceTheMetRules(t *testing.T) {
	req := createRunRequest{
		Experience: client.ExperienceBackground, NoRepositoriesOrDrives: true,
		Workload: &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: "claude-code", Task: "t"},
		Tools: []client.IncludedTool{
			harnessTool("claude-code", "", types.ToolRule{Tool: "mcp__gh__create_issue", Effect: types.ToolHold}),
			harnessTool("codex-cli", "write the tests"),
		},
	}
	f := applyRunMode(&req)
	if f == nil || f.status != http.StatusUnprocessableEntity || f.body.Reason != reasonRequestFieldUnavailable || f.body.Tool != "codex-cli" {
		t.Fatalf("refusal = %+v, want 422 request_field_unavailable for codex-cli", f)
	}
}

// TestMultiAgent_Shape pins the one shape of several agents in one background run.
func TestMultiAgent_Shape(t *testing.T) {
	for _, tc := range []struct{ name, body, reason string }{
		{"two agents, each with a task", twoAgents, ""},
		{"workload.agent's task given twice", `"experience":"background",` + agentTask + `,"tools":[{"id":"claude-code","kind":"harness","task":"x"},` + codexOwnTask + `],` + noRepos, reasonRunModeConflict},
		{"a second agent without a task", `"experience":"background",` + agentTask + `,"tools":[` + claudeTool + `,` + codexOwnTask + `,{"id":"gemini-cli","kind":"harness"}],` + noRepos, reasonRunModeConflict},
		{"workload.agent not included", `"experience":"background",` + agentTask + `,"tools":[` + codexOwnTask + `],` + noRepos, reasonRunModeConflict},
		{"a task in an interactive run", `"experience":"interactive","tools":[` + claudeTool + `,` + codexOwnTask + `],` + noRepos, reasonRunModeConflict},
		{"a task in a background command", `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[` + codexOwnTask + `],` + noRepos, reasonRunModeConflict},
		{"a blank task", `"experience":"background",` + agentTask + `,"tools":[` + claudeTool + `,{"id":"codex-cli","kind":"harness","task":"  "}],` + noRepos, reasonRunModeInvalid},
	} {
		var req createRunRequest
		if err := json.Unmarshal([]byte(modeBody(tc.body)), &req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := ""
		if f := applyRunMode(&req); f != nil {
			got = f.body.Reason
		}
		if got != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.name, got, tc.reason)
		}
	}
}

// TestMultiAgent_SwitchOffRefusesAtEveryDoor: off, a request giving an agent its own task
// is refused with its own reason at create, preflight and the policy preview; on, it is
// created with every agent's task on its harness row.
func TestMultiAgent_SwitchOffRefusesAtEveryDoor(t *testing.T) {
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "codex-cli": "wardyn/agent-codex-cli:local", "claude-code+codex-cli": setImage}
	for _, door := range harnessDoors {
		srv, _, _ := harnessSetFixture(t, &capStore{}, images, nil)
		w := harnessSetCall{"sub-plain", twoAgents}.ask(t, srv, door)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonMultiAgentTasksOff) {
			t.Errorf("%s with the switch off: %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonMultiAgentTasksOff)
		}
	}
	srv, st, _ := harnessSetFixture(t, &capStore{}, images, nil)
	srv.cfg.MultiAgentTasks = true
	w := harnessSetCall{"sub-plain", twoAgents}.ask(t, srv, harnessDoors[0])
	if w.Code != http.StatusCreated {
		t.Fatalf("create with the switch on: %d %s, want 201", w.Code, w.Body.String())
	}
	srv.WaitBackground()
	id := st.onlyRun(t)
	if st.images[id] != setImage {
		t.Errorf("run image = %q, want the set image", st.images[id])
	}
	rows := st.rows[id]
	if len(rows) != 2 || rows[0].Harness != "claude-code" || !rows[0].Starts || rows[0].Task != "" ||
		rows[1].Harness != "codex-cli" || rows[1].Starts || rows[1].Task != "write the tests" {
		t.Errorf("run_harnesses = %+v, want claude-code started on the run's task and codex-cli on its own", rows)
	}
	// GET /runs/{id} names every agent, the run's own first, each with its task.
	srv.cfg.Store = auditlessStore{st}
	get := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String(), govSession(t, "sub-plain", []string{"eng"}, false), "")
	var detail struct {
		Agents []runDetailAgent `json:"agents"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &detail); err != nil || get.Code != http.StatusOK {
		t.Fatalf("GET run: %d %s", get.Code, get.Body.String())
	}
	if want := []runDetailAgent{{"claude-code", "fix the build"}, {"codex-cli", "write the tests"}}; !slices.Equal(detail.Agents, want) {
		t.Errorf("GET run agents = %+v, want %+v", detail.Agents, want)
	}
	single := uuid.New()
	st.rows[single] = []types.RunHarness{{Harness: "claude-code", Starts: true}}
	if got := srv.runDetailAgents(t.Context(), types.AgentRun{ID: single, Agent: "claude-code", Task: "x"}); got != nil {
		t.Errorf("a run with no agent task of its own lists agents %+v", got)
	}
}

// auditlessStore is the fixture store with an empty audit read, which GET /runs/{id}
// makes for the run's apps.
type auditlessStore struct{ *harnessSetStore }

func (auditlessStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

// TestMultiAgent_TaskArgvStartsEveryAgent: the task exec runs the supervisor over every
// agent, the run's agent first; a run with no agent task of its own keeps today's argv.
func TestMultiAgent_TaskArgvStartsEveryAgent(t *testing.T) {
	run := types.AgentRun{Agent: "claude-code", Task: "write the change"}
	one := agentTaskArgv(run, []types.RunHarness{{Harness: "claude-code", Starts: true}})
	if !slices.Equal(one, []string{"/usr/local/bin/agent-run", "write the change"}) {
		t.Errorf("single agent argv = %q", one)
	}
	two := agentTaskArgv(run, []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "codex-cli", Task: "write the tests"}})
	if !slices.Equal(two, []string{"/usr/local/bin/agent-run", "--tasks", "claude-code", "write the change", "codex-cli", "write the tests"}) {
		t.Errorf("two agents argv = %q", two)
	}
}

// TestMultiAgent_OutputAgentStates: each agent's state comes off the supervisor's last
// status line, only for a run whose rows give an agent its own task, and ?agent= keeps
// that agent's lines alone.
func TestMultiAgent_OutputAgentStates(t *testing.T) {
	out := "wardyn-agents: claude-code=running codex-cli=running\n[codex-cli] done\nwardyn-agents: claude-code=running codex-cli=exited=3\n[claude-code] still going\n"
	srv, st, _ := harnessSetFixture(t, &capStore{}, nil, nil)
	multi, single := uuid.New(), uuid.New()
	st.rows[multi] = []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "codex-cli", Task: "t"}}
	st.rows[single] = []types.RunHarness{{Harness: "claude-code", Starts: true}}

	got := srv.runOutputAgents(t.Context(), multi, out)
	three := 3
	want := []runAgentState{{Agent: "claude-code", State: "running"}, {Agent: "codex-cli", State: "exited", ExitCode: &three}}
	if len(got) != 2 || got[0].Agent != want[0].Agent || got[0].State != want[0].State || got[0].ExitCode != nil ||
		got[1].Agent != want[1].Agent || got[1].State != want[1].State || got[1].ExitCode == nil || *got[1].ExitCode != 3 {
		t.Errorf("agents = %+v, want %+v", got, want)
	}
	if got := srv.runOutputAgents(t.Context(), single, out); got != nil {
		t.Errorf("a single-agent run printing a status line itself was shown states %+v", got)
	}
	if got := srv.runOutputAgents(t.Context(), multi, "[codex-cli] wardyn-agents: claude-code=exited=0\n"); got != nil {
		t.Errorf("an agent's own line was read as the supervisor's: %+v", got)
	}

	rec := httptest.NewRecorder()
	srv.writeRunOutput(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/output?agent=codex-cli", nil), multi, runOutputResponse{Output: out})
	var body runOutputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Output != "done" || len(body.Agents) != 2 {
		t.Errorf("?agent=codex-cli: output %q agents %+v, want its one line and both states", body.Output, body.Agents)
	}
}

// TestMultiAgent_SetupStatusReportsTheSwitch: the signed-in GET /setup/status names the
// switch's state under features, for an admin and a member alike, so the console offers
// several agents only when the server takes them.
func TestMultiAgent_SetupStatusReportsTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := newHarness(t)
		cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
		cfg.Runner = &fakeRunner{}
		cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		cfg.OIDC = &oidc.Authenticator{}
		cfg.MultiAgentTasks = on
		srv := New(cfg)
		for _, role := range []string{oidc.RoleAdmin, oidc.RoleUser} {
			code, st := decodeSetupSSO(t, srv, ssoSession(t, "sub-"+role, role+"@corp.example", role))
			if code != http.StatusOK || st.Features.MultiAgentTasks != on {
				t.Errorf("switch %v, %s: %d features.multi_agent_tasks = %v, want %v", on, role, code, st.Features.MultiAgentTasks, on)
			}
		}
	}
}
