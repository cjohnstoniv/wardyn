// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// attendedGoldenRules is the rule set internal/agentpolicy's generated goldens
// carry, so a dispatch can be held to the very bytes the pinned CLI ran under.
var attendedGoldenRules = []types.ToolRule{
	{Tool: "WebFetch", Effect: types.ToolHold},
	{Tool: "Bash", Effect: types.ToolDeny},
	{Tool: "mcp__github__create_issue", Effect: types.ToolDeny},
	{Tool: "Read", Effect: types.ToolAllow},
	{Tool: "mcp__github__get_issue", Effect: types.ToolAllow},
	{Tool: "Read", Effect: types.ToolAllow},
	{Tool: "*", Effect: types.ToolHold},
}

// attendedDispatch dispatches one claude-code run at level with p's posture,
// rules and harness set, and returns what the runner was handed, the
// run.agent_policy.write row (nil when none) and the run's store.
func attendedDispatch(t *testing.T, fr *fakeRunner, level types.AutonomyLevel, p dispatchParams) (runner.SandboxSpec, *agentPolicyDatum, *dispatchTestStore) {
	t.Helper()
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Agent, run.AutonomyLevel = "claude-code", level
	p.RunToken, p.Image = "run-token", "wardyn/claude-code:latest"
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
	ev := findAudit(audit.snapshot(), run.ID, "run.agent_policy.write", "success")
	if ev == nil {
		return fr.lastSpec, nil, st
	}
	var data agentPolicyDatum
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return fr.lastSpec, &data, st
}

func managedSettingsOf(spec runner.SandboxSpec) string {
	for _, f := range spec.ManagedFiles {
		if f.Path == agentpolicy.ClaudeCodeManagedSettingsPath {
			return string(f.Content)
		}
	}
	return ""
}

// TestAgentPolicyDispatch_AttendedRulesInManagedSettings: an interactive
// Claude Code run's tool rules reach the runner as the managed settings the
// pinned CLI was exercised with, byte for byte: in its level's document, in
// the attended base where the level brings none, from its own set when the
// run carries one per harness. A background run's rules stay the gate's.
func TestAgentPolicyDispatch_AttendedRulesInManagedSettings(t *testing.T) {
	golden := func(name string) string {
		b, err := os.ReadFile("../agentpolicy/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	withRules := types.RunPolicySpec{ToolRules: attendedGoldenRules}
	for _, c := range []struct {
		name   string
		level  types.AutonomyLevel
		p      dispatchParams
		golden string
	}{
		{"no level", "", dispatchParams{Interactive: true, Policy: withRules}, "attended-rules/no-level.json"},
		{"L3", types.AutonomyL3, dispatchParams{Interactive: true, Policy: withRules}, "attended-rules/no-level.json"},
		{"L1", types.AutonomyL1, dispatchParams{Interactive: true, Policy: withRules}, "attended-rules/L1.json"},
		{"L2", types.AutonomyL2, dispatchParams{Interactive: true, Policy: withRules}, "attended-rules/L2.json"},
		{"a harness set", "", dispatchParams{Interactive: true, Policy: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Edit", Effect: types.ToolDeny}}},
			Harnesses: []types.RunHarness{{Harness: "codex-cli", Starts: true}, {Harness: "claude-code"}},
			HarnessToolRules: []types.HarnessToolRules{{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "mcp__x__y", Effect: types.ToolDeny}}},
				{Harness: "claude-code", Default: types.ToolHold, Rules: attendedGoldenRules}}}, "attended-rules/no-level.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, data, st := attendedDispatch(t, &fakeRunner{}, c.level, c.p)
			if got, want := managedSettingsOf(spec), golden(c.golden); got != want {
				t.Fatalf("managed settings = %s, want the %s golden %s (run %s, hint %q)", got, c.golden, want, st.state, st.failureHint)
			}
			if data == nil || !data.Delivered || data.ToolRules != 5 || data.Bytes != len(golden(c.golden)) {
				t.Errorf("run.agent_policy.write = %+v, want delivered with tool_rules 5 (distinct names)", data)
			}
		})
	}
	spec, data, _ := attendedDispatch(t, &fakeRunner{}, "", dispatchParams{Policy: withRules})
	if managedSettingsOf(spec) != "" || data != nil {
		t.Errorf("a background run's rules were written into managed settings: %q %+v", managedSettingsOf(spec), data)
	}
	spec, _, _ = attendedDispatch(t, &fakeRunner{}, types.AutonomyL1, dispatchParams{ToolApprovals: "hold", Policy: withRules})
	if got := managedSettingsOf(spec); got != golden("L1-managed-settings.json") {
		t.Errorf("a hold-lane run's managed settings = %s, want the frozen L1 document: the gate answers its rules", got)
	}
}

// TestAgentPolicyDispatch_AttendedRulesFailClosed: an interactive run whose
// rules cannot be written exactly, or whose runner cannot deliver the
// document that carries them, is never launched without them.
func TestAgentPolicyDispatch_AttendedRulesFailClosed(t *testing.T) {
	pattern := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash(rm:*)", Effect: types.ToolDeny}}}
	for _, c := range []struct {
		name string
		fr   *fakeRunner
		p    dispatchParams
		hint string
	}{
		{"a rule Claude Code would read as a pattern", &fakeRunner{}, dispatchParams{Interactive: true, Policy: pattern},
			`tool rules could not be written into Claude Code's managed settings: tool rule "Bash(rm:*)"`},
		{"a runner that cannot deliver the file", &fakeRunner{noManagedFiles: true}, dispatchParams{Interactive: true, Policy: types.RunPolicySpec{ToolRules: attendedGoldenRules}},
			"managed settings enforce its tool rules in an interactive run, and this runner cannot vouch for them: runner"},
		{"a runtime whose guest may run as root (krun)", &fakeRunner{capsResolved: map[types.ConfinementClass]string{types.CC1: "oci/krun"}},
			dispatchParams{Interactive: true, Policy: types.RunPolicySpec{ToolRules: attendedGoldenRules}},
			"cannot vouch for them: unverified on this runtime: libkrun may run the guest as root"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, data, st := attendedDispatch(t, c.fr, "", c.p)
			if c.fr.createCalls != 0 || st.state != types.RunFailed || !strings.Contains(st.failureHint, c.hint) || data != nil {
				t.Errorf("creates %d state %s hint %q row %+v; want FAILED before any sandbox with %q", c.fr.createCalls, st.state, st.failureHint, data, c.hint)
			}
		})
	}
	// The same pattern on a background run is the gate's exact name, as before.
	fr := &fakeRunner{}
	if _, _, st := attendedDispatch(t, fr, "", dispatchParams{Policy: pattern}); fr.createCalls != 1 || st.state == types.RunFailed {
		t.Errorf("a background run with the rule was not launched: %s %q", st.state, st.failureHint)
	}
}

// TestToolPolicy_AttendedRulesReachClaudeCodeSettings is the whole path at the
// create door: an interactive run's rules for Claude Code are accepted, shown
// as Claude Code's to enforce, and dispatched into its managed settings; its
// "Run it" default is refused with the sentence step 5 greys it with.
func TestToolPolicy_AttendedRulesReachClaudeCodeSettings(t *testing.T) {
	srv, _, _ := harnessSetFixture(t, &capStore{}, composeAgentImages, nil)
	fr := srv.cfg.Runner.(*fakeRunner)
	rules := `{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"deny"},{"tool":"WebFetch","effect":"hold"}]}`
	call := harnessSetCall{"sub-plain", `"experience":"interactive","tools":[` + rules + `],` + noRepos}
	w := call.ask(t, srv, "/api/v1/runs/preflight")
	var facts struct {
		Access struct{ Harnesses []client.ToolPolicyFact }
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &facts) != nil || len(facts.Access.Harnesses) != 1 {
		t.Fatalf("preflight = %d %s", w.Code, w.Body.String())
	}
	if f := facts.Access.Harnesses[0]; f.Lane != toolLaneAttended || f.Enforcer != toolEnforcerHarness {
		t.Errorf("tool facts = %+v, want the attended lane enforced by the harness's settings", f)
	}
	if w := call.ask(t, srv, "/api/v1/runs"); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	srv.WaitBackground()
	var doc struct {
		Permissions struct{ Ask, Deny []string }
		Only        bool `json:"allowManagedPermissionRulesOnly"`
	}
	if err := json.Unmarshal([]byte(managedSettingsOf(fr.lastSpec)), &doc); err != nil || !doc.Only ||
		strings.Join(doc.Permissions.Deny, ",") != "Bash" || strings.Join(doc.Permissions.Ask, ",") != "WebFetch" {
		t.Fatalf("dispatched managed settings %q (%v), want deny Bash and ask WebFetch, locked", managedSettingsOf(fr.lastSpec), err)
	}
	// A name Claude Code would read wider than one tool is refused at every door,
	// before a 201, not at dispatch after one.
	server := harnessSetCall{"sub-plain", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"mcp__github","effect":"deny"}]}],` + noRepos}
	for _, door := range harnessDoors {
		if w := server.ask(t, srv, door); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `can't be written into Claude Code's managed settings: tool rule \"mcp__github\"`) {
			t.Errorf("%s: a whole-server rule = %d %s, want 422 before the run exists", door, w.Code, w.Body.String())
		}
	}
	skip := harnessSetCall{"sub-plain", `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","default_effect":"allow"}],` + noRepos}
	for _, door := range harnessDoors {
		if w := skip.ask(t, srv, door); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), accessSentences["attended_rules_unavailable"]) {
			t.Errorf("%s: a Run it default = %d %s, want 422 with the attended sentence", door, w.Code, w.Body.String())
		}
	}
}

// TestToolPolicy_AttendedToolNamesRefusedAtTheFold: a policy's or a person's
// name that Claude Code's settings could not carry exactly refuses an
// interactive Claude Code run at the fold; any other run keeps the name as the
// gate's exact match.
func TestToolPolicy_AttendedToolNamesRefusedAtTheFold(t *testing.T) {
	interactive := createRunRequest{Agent: "claude-code", Interactive: true}
	for _, c := range []struct {
		name  string
		req   createRunRequest
		rules []types.ToolRule
		want  bool
	}{
		{"a policy pattern", interactive, []types.ToolRule{{Tool: "Bash(rm:*)", Effect: types.ToolDeny}}, true},
		{"a policy's whole MCP server", interactive, []types.ToolRule{{Tool: "mcp__github", Effect: types.ToolAllow}}, true},
		{"a person's whole MCP server on a non-primary Claude Code", createRunRequest{Agent: "codex-cli", Interactive: true, Tools: []client.IncludedTool{
			{ID: "codex-cli", Kind: client.IncludedToolHarness}, {ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "mcp__github", Effect: types.ToolDeny}}}}}, nil, true},
		{"exact names and the default", interactive, []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "mcp__github__get_issue", Effect: types.ToolAllow}, {Tool: "*", Effect: types.ToolDeny}}, false},
		{"a background run", createRunRequest{Agent: "claude-code", Task: "t"}, []types.ToolRule{{Tool: "mcp__github", Effect: types.ToolDeny}}, false},
		{"an interactive run without Claude Code", createRunRequest{Agent: "codex-cli", Interactive: true}, []types.ToolRule{{Tool: "mcp__github", Effect: types.ToolDeny}}, false},
	} {
		refusal := attendedToolNamesRefusal(c.req, types.RunPolicySpec{ToolRules: c.rules})
		if got := refusal != nil; got != c.want || (got && refusal.body.Reason != reasonRequestFieldUnavailable) {
			t.Errorf("%s: refusal %+v, want refused=%v", c.name, refusal, c.want)
		}
	}
}
