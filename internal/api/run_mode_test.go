// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
		{"experience unknown", `"experience":"batch"`, reasonRunModeInvalid, "", 400},
		{"background needs a workload", `"experience":"background",` + noRepos, reasonRunModeRequired, runWorkloadRequiredMsg(), 400},
		{"background agent task is honoured", bgAgentTask, "", "", 200},
		{"background agent task without its tool is honoured", `"experience":"background",` + agentTask + `,` + noRepos, "", "", 200},
		{"workload kind unknown", `"experience":"background","workload":{"kind":"service"},` + noRepos, reasonRunModeInvalid, "", 400},
		{"an agent task needs a task", `"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":" "},` + noRepos, reasonRunModeInvalid, "", 400},
		{"a command carries no agent", `"experience":"background","workload":{"kind":"command","command":"make","agent":"claude-code"},` + noRepos, reasonRunModeInvalid, "", 400},
		{"background has no startup", `"experience":"background",` + agentTask + `,"startup":{"kind":"none"},` + noRepos, reasonRunModeConflict, "", 422},
		{"background has no web gateway", `"experience":"background",` + agentTask + `,"inline_policy":{"ui_apps":[{"name":"ide","port":8080}]},` + noRepos, reasonRunModeConflict, "", 422},
		{"background runs only the harness it names", `"experience":"background",` + agentTask + `,"tools":[` + codexTool + `],` + noRepos, reasonRunModeConflict, "", 422},
		{"background command includes no tool", `"experience":"background","workload":{"kind":"command","command":"make"},"tools":[` + claudeTool + `],` + noRepos, reasonRunModeConflict, "", 422},
		{"interactive has no workload", `"experience":"interactive",` + agentTask + `,"tools":[` + claudeTool + `],` + noRepos, reasonRunModeConflict, "", 422},

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
		{"an interactive environment with no tool waits for its lane", `"experience":"interactive",` + noRepos, reasonRequestFieldUnavailable, "", 422},
		{"two tools are not dropped", `"experience":"interactive","tools":[` + claudeTool + `,` + codexTool + `],` + noRepos, reasonRequestFieldUnavailable, "", 422},
		{"per-tool rules are not dropped", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"hold"}]}],` + noRepos, reasonRequestFieldUnavailable, "", 422},
		{"a per-tool default is not dropped", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"deny"}],` + noRepos, reasonRequestFieldUnavailable, "", 422},
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
		{"a start folder inside an attachment is not dropped", `"experience":"interactive","tools":[` + claudeTool + `],"drive":{"enabled":true},"start_folder":{"kind":"attachment","attachment":"drive","subpath":"src"}`, reasonRequestFieldUnavailable, "", 422},
		{"a start folder resolves under the mount's target", `"experience":"interactive","tools":[` + claudeTool + `],"workspaces":[{"workspace_id":"` + wsID + `","target":"/home/agent/work"}],"start_folder":{"kind":"attachment","attachment":"` + wsID + `","subpath":"a/../../../etc"}`, reasonStartFolderInvalid, "", 400},
		{"a start folder kind is closed", `"experience":"interactive","tools":[` + claudeTool + `],"start_folder":{"kind":"home"},` + noRepos, reasonStartFolderInvalid, "", 400},
	} {
		for _, door := range contractDoors {
			if tc.status == 200 && door.name == "create" {
				continue
			}
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
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
			if tc.name == "background command" {
				// The projected command meets the older engine's own rule for task_mode exec.
				if msg := agentRequirementError(got); msg == "" || got.TaskMode != "exec" {
					t.Fatalf("a projected command is not an exec run: %q %+v", msg, got)
				}
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
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.BlockStmt{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
					out[fn.Name.Name] = fn.Body
				}
			}
		}
	}
	return out
}

func callsBackgroundPredicate(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "backgroundOnlyRefusal" || id.Name == "refuseBackgroundOnly") {
			found = true
		}
		return !found
	})
	return found
}

// TestBackgroundDoorsAreHonest holds the door table to the code: every symbol an
// owner is told to edit exists, and Enforced is true exactly when one of the
// door's functions asks the predicate. The day a lane wires its door, this fails
// until the table says so; a door cannot be listed as denied when it is not.
func TestBackgroundDoorsAreHonest(t *testing.T) {
	bodies := backgroundDoorBodies(t)
	seen := map[string]bool{}
	for _, door := range backgroundDoors {
		if door.Door == "" || door.Owner == "" || seen[door.Door] {
			t.Errorf("door %+v needs one name and an owner", door)
		}
		seen[door.Door] = true
		enforced := false
		for _, sym := range door.Symbols {
			body, ok := bodies[sym]
			if !ok {
				t.Errorf("%s: %s is not declared (renamed or removed: update backgroundDoors)", door.Door, sym)
				continue
			}
			enforced = enforced || callsBackgroundPredicate(body)
		}
		if enforced != door.Enforced {
			t.Errorf("%s: Enforced = %v but the door's code %s the predicate", door.Door, door.Enforced, map[bool]string{true: "asks", false: "does not ask"}[enforced])
		}
	}
	for _, want := range []string{"terminal", "interactive exec", "SSH access", "web application gateway", "desktop", "interactive session"} {
		if !seen[want] {
			t.Errorf("the door table lost %q", want)
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
		"RUN_MODE_REQUIRED":        func([]string) string { return runModeRequiredMsg() },
		"RUN_WORKLOAD_REQUIRED":    func([]string) string { return runWorkloadRequiredMsg() },
		"RUN_MODE_CONFLICT":        func(a []string) string { return runModeConflictMsg(a[0], a[1]) },
		"STARTUP_TOOL_UNKNOWN":     func(a []string) string { return startupToolUnknownMsg(a[0]) },
		"START_FOLDER_ATTACHMENT":  func(a []string) string { return startFolderAttachmentMsg(a[0]) },
		"START_FOLDER_SHAPE":       func(a []string) string { return startFolderShapeMsg(a[0]) },
		"NO_REPOSITORIES_CONFLICT": func([]string) string { return noRepositoriesConflictMsg() },
		"FIELD_UNAVAILABLE":        func(a []string) string { return runModeUnavailableMsg(a[0]) },
		"BACKGROUND_ONLY":          func(a []string) string { return runBackgroundOnlyMsg(a[0]) },
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
	for _, reason := range []string{reasonRunModeRequired, reasonRunModeInvalid, reasonRunModeConflict, reasonStartFolderInvalid, reasonRunBackgroundOnly} {
		if !strings.Contains(string(ts), `"`+reason+`"`) {
			t.Errorf("run-mode-refusals.ts does not carry the reason %q", reason)
		}
	}
}
