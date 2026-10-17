// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func modeBody(extra string) string { return `{"title":"t",` + extra + `}` }

const (
	claudeTool    = `{"id":"claude-code","kind":"harness"}`
	codexTool     = `{"id":"codex-cli","kind":"harness"}`
	agentTask     = `"workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"}`
	noRepos       = `"no_repositories_or_drives":true`
	bgAgentTask   = `"experience":"background",` + agentTask + `,"tools":[` + claudeTool + `],` + noRepos
	interactiveOK = `"experience":"interactive","tools":[` + claudeTool + `],` + noRepos
	wsID          = "11111111-1111-1111-1111-111111111111"
	listedDrive   = "22222222-2222-2222-2222-222222222222"
)

// TestRunModeRefusals pins every refusal of the run-mode contract at all three
// doors, by status and reason, and the sentences the console shares.
func TestRunModeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason, msg string
		status                  int
	}{
		{"a new client's mode is never inferred", `"workload":{"kind":"command","command":"make"}`, reasonRunModeRequired, runModeRequiredMsg(), 400},
		{"start folder alone is a new client's too", `"start_folder":{"kind":"image_default"}`, reasonRunModeRequired, runModeRequiredMsg(), 400},
		{"no repositories alone is a new client's too", noRepos, reasonRunModeRequired, runModeRequiredMsg(), 400},
		{"the contract marker alone is a new client's", `"mode_contract":"0.9"`, reasonRunModeRequired, runModeRequiredMsg(), 400},
		{"the marker with only older fields is not an older client's", `"mode_contract":"0.9","agent":"claude-code","task":"t"`, reasonRunModeRequired, runModeRequiredMsg(), 400},
		{"the marker is closed", `"mode_contract":"1.0"`, reasonRunModeInvalid, "", 400},
		{"the marker beside a chosen mode is honoured", `"mode_contract":"0.9",` + bgAgentTask, "", "", 200},
		{"experience unknown", `"experience":"batch"`, reasonRunModeInvalid, "", 400},
		{"background needs a workload", `"experience":"background",` + noRepos, reasonRunModeRequired, runWorkloadRequiredMsg(), 400},
		{"background agent task is honoured", bgAgentTask, "", "", 200},
		{"background agent task without its tool is honoured", `"experience":"background",` + agentTask + `,` + noRepos, "", "", 200},
		{"workload kind unknown", `"experience":"background","workload":{"kind":"service"},` + noRepos, reasonRunModeInvalid, "", 400},
		{"an agent task needs a task", `"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":" "},` + noRepos, reasonRunModeInvalid, "", 400},
		{"a command carries no agent", `"experience":"background","workload":{"kind":"command","command":"make","agent":"claude-code"},` + noRepos, reasonRunModeInvalid, "", 400},
		{"background has no startup", `"experience":"background",` + agentTask + `,"startup":{"kind":"none"},` + noRepos, reasonRunModeConflict, "", 422},
		{"background has no idle stop", `"experience":"background",` + agentTask + `,"inline_policy":{"auto_stop_after_sec":600},` + noRepos, reasonRunModeConflict, idleStopBackgroundMsg(), 422},
		{"nor an idle stop of never", `"experience":"background",` + agentTask + `,"inline_policy":{"auto_stop_after_sec":-1},` + noRepos, reasonRunModeConflict, idleStopBackgroundMsg(), 422},
		{"an interactive environment keeps its idle stop", interactiveOK + `,"inline_policy":{"auto_stop_after_sec":600,"allowed_domains":["a.example.com"],"first_use_approval":"always_deny","min_confinement_class":"CC2"}`, "", "", 200},
		{"background has no web gateway", `"experience":"background",` + agentTask + `,"inline_policy":{"ui_apps":[{"name":"ide","port":8080}]},` + noRepos, reasonRunModeConflict, "", 422},
		{"background runs only the harness it names", `"experience":"background",` + agentTask + `,"tools":[` + codexTool + `],` + noRepos, reasonRunModeConflict, "", 422},
		{"a background command's harness takes no tool rules", `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"deny"}]}],` + noRepos, reasonRequestFieldUnavailable, runModeUnavailableMsg("tools[0]: tool rules in a background command"), 422},
		{"nor a default", `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[{"id":"claude-code","kind":"harness","default_effect":"hold"}],` + noRepos, reasonRequestFieldUnavailable, runModeUnavailableMsg("tools[0]: tool rules in a background command"), 422},
		{"a background command may include a harness", `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[` + claudeTool + `],` + noRepos, "", "", 200},
		{"interactive has no workload", `"experience":"interactive",` + agentTask + `,"tools":[` + claudeTool + `],` + noRepos, reasonRunModeConflict, "", 422},

		{"a legacy model provider does not ride beside the carriers", `"experience":"background",` + agentTask + `,"model_provider":"bedrock-team",` + noRepos, reasonRunModeConflict, runModeConflictMsg("model_provider", "Use tools[].model_provider."), 422},
		{"older fields do not ride beside the carriers", `"experience":"interactive","tools":[` + claudeTool + `],"agent":"claude-code",` + noRepos, reasonRunModeConflict,
			runModeConflictMsg("agent", "Name the harness in workload.agent or in tools."), 422},
		{"the older interactive flag does not either", `"experience":"interactive","tools":[` + claudeTool + `],"interactive":true,` + noRepos, reasonRunModeConflict, "", 422},
		{"nor tool_approvals", `"experience":"interactive","tools":[` + claudeTool + `],"tool_approvals":"hold",` + noRepos, reasonRunModeConflict, "", 422},
		{"nor run-wide tool rules", `"experience":"interactive","tools":[` + claudeTool + `],"inline_policy":{"tool_rules":[{"tool":"Bash","effect":"hold"}]},` + noRepos, reasonRunModeConflict, "", 422},
		{"nor run-wide push rules", `"experience":"interactive","tools":[` + claudeTool + `],"inline_policy":{"push_rules":{"deny_paths":["a/"]}},` + noRepos, reasonRunModeConflict, "", 422},
		{"nor the agent override's tool rules", `"experience":"interactive","tools":[` + claudeTool + `],"overrides":{"agent":{"tool_rules":[{"tool":"Bash","effect":"hold"}]}},` + noRepos, reasonRunModeConflict, "", 422},

		{"one harness, nothing starts", interactiveOK, "", "", 200},
		{"one harness starts", interactiveOK + `,"startup":{"kind":"harness","tool":"claude-code"}`, "", "", 200},
		{"a startup command", interactiveOK + `,"startup":{"kind":"command","command":"npm run dev"}`, "", "", 200},
		{"startup none", interactiveOK + `,"startup":{"kind":"none"}`, "", "", 200},
		{"a startup harness must be included", interactiveOK + `,"startup":{"kind":"harness","tool":"codex-cli"}`, reasonRunModeConflict, startupToolUnknownMsg("codex-cli"), 422},
		{"a startup is one choice: harness and command", interactiveOK + `,"startup":{"kind":"harness","tool":"claude-code","command":"x"}`, reasonRunModeInvalid, "", 400},
		{"a startup is one choice: none with a tool", interactiveOK + `,"startup":{"kind":"none","tool":"claude-code"}`, reasonRunModeInvalid, "", 400},
		{"a startup is one choice: command with a tool", interactiveOK + `,"startup":{"kind":"command","command":"x","tool":"claude-code"}`, reasonRunModeInvalid, "", 400},
		{"a startup kind is closed", interactiveOK + `,"startup":{"kind":"both"}`, reasonRunModeInvalid, "", 400},
		{"an interactive environment with no harness starts from the base image", `"experience":"interactive",` + noRepos, "", "", 200},
		{"a harness startup takes a starting prompt", interactiveOK + `,"startup":{"kind":"harness","tool":"claude-code","prompt":"fix the build"}`, "", "", 200},
		{"a starting prompt needs a harness startup", interactiveOK + `,"startup":{"kind":"none","prompt":"fix the build"}`, reasonRunModeInvalid, startupPromptShapeMsg(), 400},
		{"a command startup takes no starting prompt", interactiveOK + `,"startup":{"kind":"command","command":"make","prompt":"x"}`, reasonRunModeInvalid, startupPromptShapeMsg(), 400},
		{"two harnesses need an image declared for them", `"experience":"interactive","tools":[` + claudeTool + `,` + codexTool + `],` + noRepos, reasonHarnessSetNoImage, harnessSetNoImageMsg("Claude Code", "Codex CLI"), 422},
		{"an interactive run's tool rule goes to Claude Code's settings", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"deny"}]}],` + noRepos, "", "", 200},
		{"an interactive run's Codex CLI rule waits", `"experience":"interactive","tools":[{"id":"codex-cli","kind":"harness","tool_rules":[{"tool":"mcp__github__create_issue","effect":"deny"}]}],` + noRepos, reasonRequestFieldUnavailable, runModeUnavailableMsg("tools[0]: tool rules in an interactive run"), 422},
		{"a background run's rule is honoured on the gated lane", `"experience":"background",` + agentTask + `,"tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"hold"},{"tool":"Read","effect":"allow"}],"default_effect":"deny"}],` + noRepos, "", "", 200},
		{"a rule names one tool exactly", `"experience":"background",` + agentTask + `,"tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash(git push:*)","effect":"deny"}]}],` + noRepos, reasonRunModeInvalid, "", 400},
		{"Codex CLI cannot pause a call", `"experience":"background","workload":{"kind":"agent_task","agent":"codex-cli","task":"t"},"tools":[{"id":"codex-cli","kind":"harness","default_effect":"hold"}],` + noRepos, reasonRequestFieldUnavailable, accessSentences["harness_no_hold"], 422},
		{"Codex CLI's own tools have no block", `"experience":"background","workload":{"kind":"agent_task","agent":"codex-cli","task":"t"},"tools":[{"id":"codex-cli","kind":"harness","tool_rules":[{"tool":"shell","effect":"deny"}]}],` + noRepos, reasonRequestFieldUnavailable, codexBuiltinRuleMsg("shell"), 422},
		{"nor a Codex CLI default block", `"experience":"background","workload":{"kind":"agent_task","agent":"codex-cli","task":"t"},"tools":[{"id":"codex-cli","kind":"harness","default_effect":"deny"}],` + noRepos, reasonRequestFieldUnavailable, accessSentences["harness_no_block_builtin"], 422},
		{"your own tools take no rules", `"experience":"interactive","tools":[{"id":"none","kind":"harness","tool_rules":[{"tool":"Bash","effect":"deny"}]}],` + noRepos, reasonRequestFieldUnavailable, "Wardyn can't set tool rules for Your own tools.", 422},
		{"an interactive run's allow stands only where the policy allows the tool", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"allow"}]}],` + noRepos, reasonOverrideRefused, policyStricterMsg("Bash", types.ToolHold), 422},
		{"an interactive run's default allow has no setting", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"allow"}],` + noRepos, reasonRequestFieldUnavailable, accessSentences["attended_rules_unavailable"], 422},
		{"an interactive run's own default, asking, is honoured", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"hold"}],` + noRepos, "", "", 200},
		{"an interactive run's default block has no setting", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"deny"}],` + noRepos, reasonRequestFieldUnavailable, accessSentences["attended_rules_unavailable"], 422},
		{"a component tool is not dropped", `"experience":"interactive","tools":[{"id":"lint","kind":"component","component":{"inline":{"hosts":["a.example.com"]}}}],` + noRepos, reasonRequestFieldUnavailable, "", 422},
		{"a tool id is one token", `"experience":"interactive","tools":[{"id":"a b","kind":"harness"}],` + noRepos, reasonRunModeInvalid, "", 400},
		{"a tool is included once", `"experience":"interactive","tools":[` + claudeTool + `,` + claudeTool + `],` + noRepos, reasonRunModeInvalid, "", 400},
		{"a tool kind is closed", `"experience":"interactive","tools":[{"id":"claude-code","kind":"plugin"}],` + noRepos, reasonRunModeInvalid, "", 400},
		{"a rule named * is the default, which has its own field", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"*","effect":"hold"}]}],` + noRepos, reasonRunModeInvalid, "", 400},
		{"a tool effect is closed", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"maybe"}],` + noRepos, reasonRunModeInvalid, "", 400},
		{"a harness has no component", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","component":{"inline":{"hosts":["a.example.com"]}}}],` + noRepos, reasonRunModeInvalid, "", 400},

		{"no repositories beside a repository", interactiveOK + `,"repo":"acme/api"`, reasonRunModeConflict, noRepositoriesConflictMsg(), 422},
		{"no repositories beside a drive", interactiveOK + `,"drive":{"enabled":true}`, reasonRunModeConflict, noRepositoriesConflictMsg(), 422},
		{"a default start folder", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"image_default"},` + noRepos, "", "", 200},
		{"the image default carries nothing", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"image_default","subpath":"x"},` + noRepos, reasonStartFolderInvalid, "", 400},
		{"a start folder names an attachment of this run", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"attachment","attachment":"` + wsID + `"}`, reasonStartFolderInvalid, startFolderAttachmentMsg(wsID), 400},
		{"the drive is an attachment only when attached", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"attachment","attachment":"drive"}`, reasonStartFolderInvalid, startFolderAttachmentMsg("drive"), 400},
		{"a start folder cannot leave its mount", `"experience":"interactive","tools":[` + claudeTool + `],"drive":{"enabled":true},"start_folder":{"kind":"attachment","attachment":"drive","subpath":"../etc"}`, reasonStartFolderInvalid, startFolderShapeMsg("../etc"), 400},
		{"a start folder is relative", `"experience":"interactive","tools":[` + claudeTool + `],"drive":{"enabled":true},"start_folder":{"kind":"attachment","attachment":"drive","subpath":"/etc"}`, reasonStartFolderInvalid, "", 400},
		{"a start folder on a drive names one of drives[]", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"attachment","drive_id":"d1","subpath":"src"}`, reasonStartFolderInvalid, startFolderAttachmentMsg("d1"), 400},
		{"a listed drive the caller's grants do not reach", `"experience":"interactive","tools":[` + claudeTool + `],"drives":[{"drive_id":"` + listedDrive + `"}],"start_folder":{"kind":"attachment","drive_id":"` + listedDrive + `","subpath":"src"}`, string(authz.ReasonDriveNotFound), driveNotFoundMsg(listedDrive), 422},
		{"drive and drives are one choice", `"experience":"interactive","tools":[` + claudeTool + `],"drive":{"enabled":true},"drives":[{"drive_id":"` + listedDrive + `"}]`, reasonDriveSelectionConflict, driveSelectionConflictMsg(), 400},
		{"no repositories beside a listed drive", interactiveOK + `,"drives":[{"drive_id":"` + listedDrive + `"}]`, reasonRunModeConflict, noRepositoriesConflictMsg(), 422},
		{"a subpath cannot step out through its mount's target", `"experience":"interactive","tools":[` + claudeTool + `],"workspaces":[{"workspace_id":"` + wsID + `","target":"/home/agent/work"}],"start_folder":{"kind":"attachment","attachment":"` + wsID + `","subpath":"a/../../../etc"}`, reasonStartFolderInvalid, "", 400},
		{"an unavailable carrier answers before the older gates read the projection", `"experience":"interactive","tools":[` + claudeTool + `,{"id":"c","kind":"component","component":{"inline":{"hosts":["a.example.com"]}}}],"startup":{"kind":"command","command":"harness login"},` + noRepos, reasonRequestFieldUnavailable, "", 422},
		{"a start folder kind is closed", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"home"},` + noRepos, reasonStartFolderInvalid, "", 400},
	} {
		for _, door := range contractDoors {
			if tc.status == 200 && door.name == "create" {
				continue
			}
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				h.srv.cfg.AgentImages = composeAgentImages
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(tc.body))
				if w.Code != tc.status || errorReason(w) != tc.reason {
					t.Fatalf("got %d %s, want %d reason %q", w.Code, w.Body.String(), tc.status, tc.reason)
				}
				if tc.msg != "" {
					var body struct{ Error string }
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error != tc.msg {
						t.Fatalf("sentence = %q, want %q", body.Error, tc.msg)
					}
				}
			})
		}
	}
}

// TestRunMode_TwoHarnessesStartFromTheSetImage (T0, inverted at DS1-S4): two
// harnesses are honoured at every door, and create launches them from the
// image declared for the set, with both recorded and the started one marked.
func TestRunMode_TwoHarnessesStartFromTheSetImage(t *testing.T) {
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "claude-code+codex-cli": setImage}
	body := `"experience":"interactive","tools":[` + claudeTool + `,` + codexTool + `],"startup":{"kind":"harness","tool":"codex-cli"},` + noRepos
	for _, door := range harnessDoors {
		srv, st, _ := harnessSetFixture(t, &capStore{}, images, nil)
		w := harnessSetCall{"sub-plain", body}.ask(t, srv, door)
		if door != harnessDoors[0] {
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d %s, want 200", door, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s, want 201", w.Code, w.Body.String())
		}
		srv.WaitBackground()
		id := st.onlyRun(t)
		if st.images[id] != setImage {
			t.Errorf("run image = %q, want the declared set image %q", st.images[id], setImage)
		}
		rows := st.rows[id]
		if len(rows) != 2 || !slices.ContainsFunc(rows, func(h types.RunHarness) bool { return h.Harness == "codex-cli" && h.Starts }) ||
			!slices.ContainsFunc(rows, func(h types.RunHarness) bool { return h.Harness == "claude-code" && !h.Starts }) {
			t.Errorf("run_harnesses = %+v, want claude-code and codex-cli, codex-cli started", rows)
		}
	}
}

// TestPersonalToolRulesPutABackgroundRunOnTheGatedLane pins the posture that
// replaced the refusal of a person's rules off the gated lane: a background run
// of a gated harness with any rule or default goes on the gated lane with
// "*: allow" under the person's layer, a run already there keeps its own default,
// and nothing else moves.
func TestPersonalToolRulesPutABackgroundRunOnTheGatedLane(t *testing.T) {
	ruled := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}}}
	for name, tc := range map[string]struct {
		req       createRunRequest
		approvals string
		rules     []types.ToolRule
	}{
		"ungated background run": {createRunRequest{Agent: "claude-code", Task: "t", Tools: ruled}, "hold", []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}},
		"gated background run":   {createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold", Tools: ruled}, "hold", nil},
		"no rule":                {createRunRequest{Agent: "claude-code", Task: "t", Tools: []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}}}, "", nil},
		"interactive":            {createRunRequest{Agent: "claude-code", Interactive: true, Tools: ruled}, "", nil},
		"a gateless harness":     {createRunRequest{Agent: "codex-cli", Task: "t", Tools: ruled}, "", nil},
		"a command":              {createRunRequest{Agent: "claude-code", Task: "t", TaskMode: "exec", Tools: ruled}, "", nil},
	} {
		req, spec := tc.req, types.RunPolicySpec{}
		(&Server{}).applyToolPosture(&req, &spec)
		if req.ToolApprovals != tc.approvals || !slices.Equal(spec.ToolRules, tc.rules) {
			t.Errorf("%s: tool_approvals %q rules %v, want %q %v", name, req.ToolApprovals, spec.ToolRules, tc.approvals, tc.rules)
		}
	}
	// A policy's own "*" is never replaced: the posture lies under it.
	req, spec := createRunRequest{Agent: "claude-code", Task: "t", Tools: ruled}, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolHold}}}
	(&Server{}).applyToolPosture(&req, &spec)
	if req.ToolApprovals != "hold" || len(spec.ToolRules) != 1 || spec.ToolRules[0].Effect != types.ToolHold {
		t.Errorf("a policy default under the posture: %q %v", req.ToolApprovals, spec.ToolRules)
	}
}

// TestRunModeProjection pins the one mapping from the carriers onto the legacy
// fields the engine reads, and that it chooses nothing the person did not send.
func TestRunModeProjection(t *testing.T) {
	const provider = "bedrock-team"
	for _, tc := range []struct {
		name string
		in   createRunRequest
		want createRunRequest
	}{
		{"background agent task",
			createRunRequest{Experience: client.ExperienceBackground, NoRepositoriesOrDrives: true,
				Workload: &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: "claude-code", Task: "fix"},
				Tools:    []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: provider}}},
			createRunRequest{Agent: "claude-code", Task: "fix", ModelProvider: provider}},
		{"background command",
			createRunRequest{Experience: client.ExperienceBackground, Workload: &client.RunWorkload{Kind: client.WorkloadCommand, Command: "make test"}, NoRepositoriesOrDrives: true},
			createRunRequest{TaskMode: "exec", Task: "make test"}},
		{"interactive, nothing starts",
			createRunRequest{Experience: client.ExperienceInteractive, Tools: []client.IncludedTool{{ID: "codex-cli", Kind: client.IncludedToolHarness}}, NoRepositoriesOrDrives: true},
			createRunRequest{Interactive: true, Agent: "codex-cli"}},
		{"interactive, startup none starts nothing",
			createRunRequest{Experience: client.ExperienceInteractive, NoRepositoriesOrDrives: true,
				Tools:   []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}},
				Startup: &client.RunStartup{Kind: client.StartupNone}},
			createRunRequest{Interactive: true, Agent: "claude-code"}},
		{"interactive, the harness starts",
			createRunRequest{Experience: client.ExperienceInteractive, NoRepositoriesOrDrives: true,
				Tools:   []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: provider}},
				Startup: &client.RunStartup{Kind: client.StartupHarness, Tool: "claude-code"}},
			createRunRequest{Interactive: true, Agent: "claude-code", ModelProvider: provider, InteractiveStart: "agent"}},
		{"interactive, a command starts",
			createRunRequest{Experience: client.ExperienceInteractive, NoRepositoriesOrDrives: true,
				Tools:   []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}},
				Startup: &client.RunStartup{Kind: client.StartupCommand, Command: "npm run dev"}},
			createRunRequest{Interactive: true, Agent: "claude-code", InteractiveStart: "shell", Task: "npm run dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in
			if refusal := applyRunMode(&got); refusal != nil {
				t.Fatalf("refused: %+v", refusal.body)
			}
			if tc.name == "background command" && got.TaskMode != "exec" {
				// The projected command is an exec run; its image comes from the
				// harness set, so agentRequirementError asks for no agent.
				t.Fatalf("a projected command is not an exec run: %+v", got)
			}
			// The carriers stay on the request: the fold and the create audit read them.
			tc.want.Experience, tc.want.Workload, tc.want.Tools, tc.want.Startup, tc.want.NoRepositoriesOrDrives =
				tc.in.Experience, tc.in.Workload, tc.in.Tools, tc.in.Startup, tc.in.NoRepositoriesOrDrives
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("projected = %+v\nwant        %+v", got, tc.want)
			}
			// Running it again must not refuse: the fold re-reads the projected request.
			if refusal := (&Server{}).runContractRefusal(got); refusal != nil {
				t.Fatalf("the fold refuses a projected request: %+v", refusal.body)
			}
		})
	}
}

// TestRunModeLeavesAnOlderClientAlone is the compatibility mapping's promise: a
// request with none of the run-mode fields is not read, not changed and not
// refused, and its run stores no experience.
func TestRunModeLeavesAnOlderClientAlone(t *testing.T) {
	for _, req := range []createRunRequest{
		{Agent: "claude-code", Task: "t"},
		{Agent: "claude-code", Interactive: true},
		{Agent: "codex-cli", Task: "echo hi", TaskMode: "exec"},
		{Agent: "claude-code", Interactive: true, InteractiveStart: "agent", ToolApprovals: ""},
		{Agent: "claude-code", Task: "t", ToolApprovals: "hold", ModelProvider: "bedrock-team",
			InlinePolicy: &types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, PushRules: &types.PushRulesSpec{DenyPaths: []string{"a/"}}}},
	} {
		got := req
		if refusal := applyRunMode(&got); refusal != nil || !reflect.DeepEqual(got, req) {
			t.Errorf("%+v was changed or refused: %+v %+v", req, got, refusal)
		}
		if req.UsesRunMode() {
			t.Errorf("%+v reads as a new client's", req)
		}
	}
}

// TestRunModeIsStoredOnlyWhenChosen pins that the experience the doors read is
// the one the request carried: an older client's run keeps every door's behaviour.
func TestRunModeIsStoredOnlyWhenChosen(t *testing.T) {
	for _, tc := range []struct {
		run  types.AgentRun
		want bool
	}{
		{types.AgentRun{}, false},
		{types.AgentRun{Interactive: false, Task: "a legacy autonomous run"}, false},
		{types.AgentRun{Experience: types.ExperienceInteractive}, false},
		{types.AgentRun{Experience: types.ExperienceBackground, Interactive: true}, true},
	} {
		if got := tc.run.BackgroundOnly(); got != tc.want {
			t.Errorf("%+v: BackgroundOnly = %v, want %v", tc.run, got, tc.want)
		}
	}
}

// TestAdmissionAndTheDoorsReadOneValue pins the invariant a pool's run-type
// limit relies on: the experience admission sees is the one stored on the run and
// read by every door. A request in the older shape stores none, so a Background-only
// place refuses it; it is never admitted as a background run that the doors then
// leave attachable.
func TestAdmissionAndTheDoorsReadOneValue(t *testing.T) {
	backgroundOnlyPool := types.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{}}
	for _, tc := range []struct {
		name         string
		req          createRunRequest
		wantAdmitted bool
	}{
		{"an older client's task run", createRunRequest{Agent: "claude-code", Task: "fix the build"}, false},
		{"an older client's command run", createRunRequest{Agent: "codex-cli", Task: "make", TaskMode: "exec"}, false},
		{"a new client's background run", createRunRequest{Experience: client.ExperienceBackground, Workload: &client.RunWorkload{Kind: client.WorkloadCommand, Command: "make"}, NoRepositoriesOrDrives: true}, true},
		{"a new client's interactive run", createRunRequest{Experience: client.ExperienceInteractive, Tools: []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}}, NoRepositoriesOrDrives: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			if refusal := applyRunMode(&req); refusal != nil {
				t.Fatalf("refused: %+v", refusal.body)
			}
			stored := types.AgentRun{Experience: req.Experience}
			if got := backgroundOnlyPool.AllowsRunType(types.RunnerPoolRunType(stored.Experience)); got != tc.wantAdmitted {
				t.Errorf("a Background-only place admitted = %v, want %v", got, tc.wantAdmitted)
			}
			if admitted := backgroundOnlyPool.AllowsRunType(types.RunnerPoolRunType(stored.Experience)); admitted != stored.BackgroundOnly() {
				t.Errorf("admitted as background = %v but the doors read BackgroundOnly = %v", admitted, stored.BackgroundOnly())
			}
			bothTypes := types.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{}, Interactive: &types.RunnerPoolInteractiveLimits{}}
			if req.Experience == "" && bothTypes.AllowsRunType(types.RunnerPoolRunType(stored.Experience)) {
				t.Error("an unset experience satisfied a limit that lists both run types")
			}
		})
	}
}

// TestBackgroundIdleStopIsBoundWhateverTheSource: a resolved policy's idle stop
// is zeroed for a background run (with a note), left alone for everything else,
// and the idle sweep never takes a background run as a candidate.
func TestBackgroundIdleStopIsBoundWhateverTheSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exp      client.Experience
		in, want int
		noted    bool
	}{
		{"background, a policy's idle stop", client.ExperienceBackground, 1800, 0, true},
		{"background, an explicit never", client.ExperienceBackground, -1, 0, true},
		{"background, none", client.ExperienceBackground, 0, 0, false},
		{"interactive keeps it", client.ExperienceInteractive, 1800, 1800, false},
		{"an older client keeps it", "", 1800, 1800, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := types.RunPolicySpec{AutoStopAfterSec: tc.in}
			notes := boundBackgroundIdleStop(createRunRequest{Experience: tc.exp}, &spec)
			if spec.AutoStopAfterSec != tc.want || (len(notes) > 0) != tc.noted {
				t.Errorf("idle stop = %d notes %v, want %d noted=%v", spec.AutoStopAfterSec, notes, tc.want, tc.noted)
			}
		})
	}
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		run  types.AgentRun
		want bool
	}{
		{types.AgentRun{AutoStopAfterSec: 60, UpdatedAt: old}, true},
		{types.AgentRun{AutoStopAfterSec: 60, UpdatedAt: old, Experience: types.ExperienceInteractive}, true},
		{types.AgentRun{AutoStopAfterSec: 60, UpdatedAt: old, Experience: types.ExperienceBackground}, false},
	} {
		if got := autoStopCandidate(tc.run, time.Now()); got != tc.want {
			t.Errorf("%+v: candidate = %v, want %v", tc.run, got, tc.want)
		}
	}
}

// TestNoRepositoriesIsHeldToTheResolvedPolicy: the explicit choice of nothing
// attached is refused when the policy the run resolves to mounts or clones
// something, whichever source brought it.
func TestNoRepositoriesIsHeldToTheResolvedPolicy(t *testing.T) {
	mount := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/data", Target: "/work"}}}
	repo := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "acme/widgets", Target: "/work"}}}
	for _, tc := range []struct {
		name string
		req  createRunRequest
		spec types.RunPolicySpec
		want bool
	}{
		{"nothing resolved", createRunRequest{NoRepositoriesOrDrives: true}, types.RunPolicySpec{}, false},
		{"a resolved mount", createRunRequest{NoRepositoriesOrDrives: true}, mount, true},
		{"a resolved clone", createRunRequest{NoRepositoriesOrDrives: true}, repo, true},
		{"no explicit choice", createRunRequest{}, mount, false},
	} {
		refusal := noRepositoriesResolvedRefusal(tc.req, tc.spec)
		if (refusal != nil) != tc.want || (refusal != nil && (refusal.body.Reason != reasonRunModeConflict || refusal.body.Error != noRepositoriesConflictMsg())) {
			t.Errorf("%s: refusal = %+v, want refused=%v with the shared sentence", tc.name, refusal, tc.want)
		}
	}
}

// TestHarnessRulesAreAvailableOnceTheHarnessIsBound pins the other side of the
// dependency types.HarnessToolRules documents: the proxy now names the harness of
// a call from a per-harness credential, so a second included tool, a per-tool
// rule and a per-tool default are honoured; a component tool still is not.
func TestHarnessRulesAreAvailableOnceTheHarnessIsBound(t *testing.T) {
	claude := client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness}
	codex := client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness}
	held := claude
	held.DefaultEffect = types.ToolHold
	ruled := claude
	ruled.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}
	for name, req := range map[string]createRunRequest{
		"one plain harness":        {Tools: []client.IncludedTool{claude}},
		"a second included tool":   {Tools: []client.IncludedTool{claude, codex}},
		"a per-tool default":       {Tools: []client.IncludedTool{held}},
		"a per-tool rule":          {Tools: []client.IncludedTool{ruled}},
		"a rule on the second one": {Tools: []client.IncludedTool{claude, ruled}},
	} {
		if refusal := runModeUnavailableRefusal(req); refusal != nil {
			t.Errorf("%s: refused: %+v", name, refusal)
		}
	}
	comp := client.IncludedTool{ID: "c", Kind: client.IncludedToolComponent, Component: &client.ComponentRef{Inline: &types.ComponentDefinition{Hosts: []string{"a.example.com"}}}}
	if refusal := runModeUnavailableRefusal(createRunRequest{Tools: []client.IncludedTool{claude, comp}}); refusal == nil || refusal.body.Reason != reasonRequestFieldUnavailable {
		t.Errorf("a component tool is still unavailable: %+v", refusal)
	}
}

// TestNoRepositoriesIsRefusedAtEveryDoorWhenThePolicyMountsSomething drives the
// resolve step itself: a deployment default that mounts a directory meets the
// explicit choice of nothing attached with the shared refusal, at create,
// preflight and preview, and the older client's launch under that default is
// unchanged.
func TestNoRepositoriesIsRefusedAtEveryDoorWhenThePolicyMountsSomething(t *testing.T) {
	for _, door := range contractDoors {
		t.Run(door.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.DefaultPolicy.WorkspaceMounts = []types.WorkspaceMount{{Source: "/data", Target: "/home/agent/work"}}
			w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(interactiveOK))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunModeConflict || !strings.Contains(w.Body.String(), noRepositoriesConflictMsg()) {
				t.Fatalf("got %d %s, want 422 %s", w.Code, w.Body.String(), reasonRunModeConflict)
			}
		})
	}
}

func TestBackgroundOnlyRefusal(t *testing.T) {
	bg := types.AgentRun{Experience: types.ExperienceBackground}
	refusal := backgroundOnlyRefusal(bg, "terminal")
	if refusal == nil || refusal.status != http.StatusConflict || refusal.body.Reason != reasonRunBackgroundOnly ||
		refusal.body.Error != runBackgroundOnlyMsg("terminal") {
		t.Fatalf("a background run's door answered %+v", refusal)
	}
	for _, run := range []types.AgentRun{{}, {Interactive: false}, {Experience: types.ExperienceInteractive, Interactive: true}} {
		if backgroundOnlyRefusal(run, "terminal") != nil {
			t.Errorf("%+v must be enterable", run)
		}
	}
	rec := httptest.NewRecorder()
	if !(&Server{}).refuseBackgroundOnly(rec, httptest.NewRequest(http.MethodGet, "/", nil), bg, "SSH access") || rec.Code != http.StatusConflict {
		t.Fatalf("refuseBackgroundOnly = %d %s", rec.Code, rec.Body.String())
	}
}

// backgroundDoorBodies parses the package's own non-test files and returns each
// declared function's body, by name, for the door table's guard.
func backgroundDoorBodies(t *testing.T) map[string]*ast.BlockStmt {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.BlockStmt{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				out[fn.Name.Name] = fn.Body
			}
		}
	}
	return out
}

// callsBackgroundPredicate reports whether body CALLS the predicate (a bare
// mention such as `_ = backgroundOnlyRefusal` does not count).
func callsBackgroundPredicate(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name == "backgroundOnlyRefusal" || name == "refuseBackgroundOnly" {
			found = true
		}
		return !found
	})
	return found
}

// doorEnforced is the table's Enforced column recomputed from the code: every
// listed symbol must call the predicate, and a door with no symbol is never enforced.
func doorEnforced(door backgroundDoor, bodies map[string]*ast.BlockStmt) bool {
	if len(door.Symbols) == 0 {
		return false
	}
	for _, sym := range door.Symbols {
		if body, ok := bodies[sym]; !ok || !callsBackgroundPredicate(body) {
			return false
		}
	}
	return true
}

// TestBackgroundDoorsAreHonest holds the door table to the code: every symbol an
// owner is told to edit exists, and Enforced is true exactly when EVERY one of the
// door's functions asks the predicate. The day a lane wires its door, this fails
// until the table says so; a door cannot be listed as denied when it is not, nor
// when only some of its entry points are wired.
func TestBackgroundDoorsAreHonest(t *testing.T) {
	bodies := backgroundDoorBodies(t)
	seen := map[string]bool{}
	for _, door := range backgroundDoors {
		if door.Door == "" || door.Owner == "" || seen[door.Door] {
			t.Errorf("door %+v needs one name and an owner", door)
		}
		seen[door.Door] = true
		for _, sym := range door.Symbols {
			if _, ok := bodies[sym]; !ok {
				t.Errorf("%s: %s is not declared (renamed or removed: update backgroundDoors)", door.Door, sym)
			}
		}
		if enforced := doorEnforced(door, bodies); enforced != door.Enforced {
			t.Errorf("%s: Enforced = %v but the door's code %s the predicate in every listed function", door.Door, door.Enforced, map[bool]string{true: "asks", false: "does not ask"}[enforced])
		}
	}
	for _, want := range []string{"terminal", "interactive exec", "SSH access", "web application gateway", "desktop"} {
		if !seen[want] {
			t.Errorf("the door table lost %q", want)
		}
	}
	for _, door := range backgroundDoors {
		for _, sym := range door.Symbols {
			if sym == "handleReviveRun" || sym == "reviveRunProxy" {
				t.Errorf("%s lists %s: revive is lifecycle recovery, not an interactive door", door.Door, sym)
			}
		}
	}
}

// TestDoorEnforcedNeedsEverySymbol pins the check TestBackgroundDoorsAreHonest
// makes: one wired function of several, a bare mention and an empty door are not
// enforcement.
func TestDoorEnforcedNeedsEverySymbol(t *testing.T) {
	parse := func(src string) *ast.BlockStmt {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f.Decls[0].(*ast.FuncDecl).Body
	}
	asks := parse(`func asks() { _ = backgroundOnlyRefusal(run, "x") }`)
	method := parse(`func method() { if s.refuseBackgroundOnly(w, r, run, "x") { return } }`)
	silent := parse(`func silent() {}`)
	mention := parse(`func mention() { _ = backgroundOnlyRefusal }`)
	for _, tc := range []struct {
		name    string
		symbols []string
		want    bool
	}{
		{"every symbol asks", []string{"asks", "method"}, true},
		{"one of two asks", []string{"asks", "silent"}, false},
		{"a bare mention is not a call", []string{"mention"}, false},
		{"no symbols is never enforced", nil, false},
		{"an undeclared symbol is not enforced", []string{"asks", "gone"}, false},
	} {
		got := doorEnforced(backgroundDoor{Symbols: tc.symbols}, map[string]*ast.BlockStmt{"asks": asks, "method": method, "silent": silent, "mention": mention})
		if got != tc.want {
			t.Errorf("%s: enforced = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// runRoutes lists every "METHOD /runs/{id}..." route the package registers, found
// by reading its own source.
func runRoutes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			lit, isLit := call.Args[0].(*ast.BasicLit)
			if !ok || !isLit || lit.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(path, "/runs/{id}") {
				return true
			}
			out[strings.ToUpper(sel.Sel.Name)+" "+path] = true
			return true
		})
	}
	return out
}

// TestRunRoutesAreClassified is the completeness guard: every /runs/{id} route
// the server registers is either a background door's route or a passive one, so
// a new route that could open a shell, a web app or a desktop cannot be added
// without deciding which. Stale entries fail too.
func TestRunRoutesAreClassified(t *testing.T) {
	registered := runRoutes(t)
	if len(registered) < 20 {
		t.Fatalf("found %d run routes; the route scan is broken", len(registered))
	}
	classified := map[string]string{}
	for route, why := range passiveRunRoutes {
		classified[route] = "passive: " + why
	}
	for _, door := range backgroundDoors {
		for _, route := range door.Routes {
			if prior, dup := classified[route]; dup {
				t.Errorf("%s is listed twice (%s, and door %s)", route, prior, door.Door)
			}
			classified[route] = "door: " + door.Door
		}
	}
	for route := range registered {
		if classified[route] == "" {
			t.Errorf("%s is registered but neither a door's route nor in passiveRunRoutes (run_mode_doors.go): decide whether it opens an interactive surface", route)
		}
	}
	for route := range classified {
		if !registered[route] {
			t.Errorf("%s is classified but no longer registered", route)
		}
	}
}

// TestRunModeRefusalSentencesMatchGolden pins the sentences to the table the
// console's twin is pinned to, and the wire reasons to the TypeScript constants.
func TestRunModeRefusalSentencesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "run-mode-refusals.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Key  string   `json:"key"`
		Args []string `json:"args"`
		Text string   `json:"text"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	got := map[string]func(a []string) string{
		"RUN_MODE_REQUIRED":          func([]string) string { return runModeRequiredMsg() },
		"RUN_WORKLOAD_REQUIRED":      func([]string) string { return runWorkloadRequiredMsg() },
		"RUN_MODE_CONFLICT":          func(a []string) string { return runModeConflictMsg(a[0], a[1]) },
		"STARTUP_TOOL_UNKNOWN":       func(a []string) string { return startupToolUnknownMsg(a[0]) },
		"START_FOLDER_ATTACHMENT":    func(a []string) string { return startFolderAttachmentMsg(a[0]) },
		"START_FOLDER_SHAPE":         func(a []string) string { return startFolderShapeMsg(a[0]) },
		"NO_REPOSITORIES_CONFLICT":   func([]string) string { return noRepositoriesConflictMsg() },
		"FIELD_UNAVAILABLE":          func(a []string) string { return runModeUnavailableMsg(a[0]) },
		"BACKGROUND_ONLY":            func(a []string) string { return runBackgroundOnlyMsg(a[0]) },
		"IDLE_STOP_BACKGROUND":       func([]string) string { return idleStopBackgroundMsg() },
		"RUN_SHAPE_CONFLICT":         func(a []string) string { return runShapeConflictMsg(a[0], a[1]) },
		"STARTUP_PROMPT_SHAPE":       func([]string) string { return startupPromptShapeMsg() },
		"HARNESS_NOT_INSTALLED":      func(a []string) string { return harnessNotInstalledMsg(a[0]) },
		"HARNESS_SET_NO_IMAGE":       func(a []string) string { return harnessSetNoImageMsg(a...) },
		"HARNESS_SET_CUSTOM_IMAGE":   func([]string) string { return harnessSetCustomImageMsg() },
		"HARNESS_PROVIDERS_CONFLICT": func(a []string) string { return harnessProvidersConflictMsg(a[0], a[1]) },
		"HARNESS_SIGNIN_NOT_PRIMARY": func(a []string) string { return harnessSignInNotPrimaryMsg(a[0], a[1]) },
		"MAIN_DRIVE_UNENFORCEABLE":   func([]string) string { return mainDriveUnenforceableMsg() },
		"CODEX_BUILTIN_RULE":         func(a []string) string { return codexBuiltinRuleMsg(a[0]) },
		"POLICY_OVERLAY_INVALID":     func(a []string) string { return policyOverlayInvalidMsg(a[0], a[1]) },
		"POLICY_OVERLAY_CONFLICT":    func([]string) string { return policyOverlayConflictMsg() },
	}
	if len(golden) != len(got) {
		t.Fatalf("golden has %d sentences, the server has %d", len(golden), len(got))
	}
	for _, g := range golden {
		fn, ok := got[g.Key]
		if !ok {
			t.Fatalf("golden names %s, which the server has no sentence for", g.Key)
		}
		if s := fn(g.Args); s != g.Text {
			t.Errorf("%s = %q, golden %q", g.Key, s, g.Text)
		}
	}
	ts, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "run-mode-refusals.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{reasonRunModeRequired, reasonRunModeInvalid, reasonRunModeConflict, reasonStartFolderInvalid, reasonRunBackgroundOnly, reasonRunShapeConflict,
		reasonHarnessNotInstalled, reasonHarnessSetNoImage, reasonHarnessSetCustomImage, reasonHarnessProvidersConflict,
		reasonPolicyOverlayInvalid, reasonPolicyOverlayConflict, reasonMainDriveUnenforceable} {
		if !strings.Contains(string(ts), `"`+reason+`"`) {
			t.Errorf("run-mode-refusals.ts does not carry the reason %q", reason)
		}
	}
}
