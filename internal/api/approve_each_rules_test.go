// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// approveEachTool is a tool name approve_each may be written on: one MCP tool's exact name.
const approveEachTool = "mcp__jira__delete_issue"

// remoteMCPComps is one remote MCP server, "Linear" (key linear), turned on for harnesses.
func remoteMCPComps(harnesses ...string) runComponents {
	def := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.linear.test/mcp"}}
	return runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "Linear", Definition: def, MCPHarnesses: harnesses}}}}
}

// approve_each is written on one MCP tool's exact name and nowhere else: not on "*", not on a
// built-in tool, not on a pattern. Every policy entry path funnels through validatePolicySpec.
func TestValidateToolRulesApproveEach(t *testing.T) {
	base := types.RunPolicySpec{MinConfinementClass: types.CC1, FirstUseApproval: types.FirstUseAlwaysDeny}
	with := func(rules ...types.ToolRule) types.RunPolicySpec {
		s := base
		s.ToolRules = rules
		return s
	}
	if err := validatePolicySpec(with(
		types.ToolRule{Tool: approveEachTool, Effect: types.ToolApproveEach},
		types.ToolRule{Tool: "Bash", Effect: types.ToolHold},
	)); err != nil {
		t.Fatalf("approve_each on an exact MCP tool name refused: %v", err)
	}
	for _, tool := range []string{"*", "Bash", "mcp__jira__*", "mcp__jira"} {
		err := validatePolicySpec(with(types.ToolRule{Tool: tool, Effect: types.ToolApproveEach}))
		if err == nil || !strings.Contains(err.Error(), "tool_rules[0]") || !strings.Contains(err.Error(), "approve_each") {
			t.Errorf("approve_each on %q: %v, want a refusal naming tool_rules[0] and approve_each", tool, err)
		}
	}
	if err := validatePolicySpec(with(types.ToolRule{Tool: "Bash", Effect: "maybe"})); err == nil {
		t.Error("an unknown effect was accepted")
	}
}

// Walls only narrow, and approve_each narrows a tool (it asks for each call), so a walls document
// may carry it on an MCP tool; the run's rules then take the stricter of what they had and it.
func TestPersonalWallsAcceptApproveEach(t *testing.T) {
	rule := types.ToolRule{Tool: approveEachTool, Effect: types.ToolApproveEach}
	if err := validatePersonalWalls(types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{rule}}); err != nil {
		t.Fatalf("walls with approve_each on an MCP tool refused: %v", err)
	}
	err := validatePersonalWalls(types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolApproveEach}}})
	if err == nil || !strings.HasPrefix(err.Error(), "tool_rules[0]") {
		t.Errorf("walls with approve_each on Bash: %v, want a refusal naming tool_rules[0]", err)
	}
	doc := []types.ToolRule{rule}
	for name, tc := range map[string]struct {
		base []types.ToolRule
		want types.ToolEffect
	}{
		"no rule before":        {nil, types.ToolApproveEach},
		"a hold before":         {[]types.ToolRule{{Tool: approveEachTool, Effect: types.ToolHold}}, types.ToolApproveEach},
		"an allow before":       {[]types.ToolRule{{Tool: approveEachTool, Effect: types.ToolAllow}}, types.ToolApproveEach},
		"a deny before":         {[]types.ToolRule{{Tool: approveEachTool, Effect: types.ToolDeny}}, types.ToolDeny},
		"approve_each before":   {[]types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}, types.ToolApproveEach},
		"a default hold before": {[]types.ToolRule{{Tool: "*", Effect: types.ToolHold}}, types.ToolApproveEach},
	} {
		if got := effectiveToolEffect(narrowToolRules(tc.base, doc), approveEachTool); got != tc.want {
			t.Errorf("%s: walls compose to %q, want %q", name, got, tc.want)
		}
	}
}

// A run contract's per-run agent rules take approve_each on an MCP tool, and refuse it elsewhere.
func TestRunContractOverridesApproveEach(t *testing.T) {
	ok := client.AgentOverrides{ToolRules: []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}}
	if got := agentOverridesShape(ok); got != nil {
		t.Fatalf("approve_each on an MCP tool refused: %+v", got.body)
	}
	for _, tool := range []string{"*", "Bash"} {
		bad := client.AgentOverrides{ToolRules: []types.ToolRule{{Tool: tool, Effect: types.ToolApproveEach}}}
		if got := agentOverridesShape(bad); got == nil {
			t.Errorf("approve_each on %q accepted", tool)
		}
	}
}

// approve_each is a rule for one MCP tool, never a harness default.
func TestRunModeDefaultEffectRefusesApproveEach(t *testing.T) {
	got := toolRulesShapeRefusal(0, client.IncludedTool{ID: claudeCodeHarness, Kind: client.IncludedToolHarness, DefaultEffect: types.ToolApproveEach})
	if got == nil || !strings.Contains(got.body.Error, "default_effect") {
		t.Fatalf("approve_each as a default: %+v, want a refusal naming default_effect", got)
	}
	rule := client.IncludedTool{ID: claudeCodeHarness, Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}}
	if got := toolRulesShapeRefusal(0, rule); got != nil {
		t.Errorf("approve_each on an MCP tool refused: %+v", got.body)
	}
	rule.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolApproveEach}}
	if got := toolRulesShapeRefusal(0, rule); got == nil {
		t.Error("approve_each on Bash accepted")
	}
}

// The order is allow < hold < approve_each < deny, with no opinion below allow, and every rank site
// reads it: the stricter of two, and a rule that narrows a tool.
func TestEffectRankApproveEach(t *testing.T) {
	a, h, e, d := types.ToolAllow, types.ToolHold, types.ToolApproveEach, types.ToolDeny
	for _, tc := range []struct{ a, b, want types.ToolEffect }{
		{a, h, h}, {h, a, h}, {h, e, e}, {e, h, e}, {a, e, e}, {e, d, d}, {d, e, d}, {e, e, e},
		{h, "maybe", "maybe"}, // an effect this build does not know ranks with deny, never below hold
	} {
		if got := stricterToolEffect(tc.a, tc.b); got != tc.want {
			t.Errorf("stricterToolEffect(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	rules := []types.ToolRule{{Tool: approveEachTool, Effect: h}}
	out, replaced, applied := narrowToolRule(rules, types.ToolRule{Tool: approveEachTool, Effect: e})
	if !applied || replaced != h || effectiveToolEffect(out, approveEachTool) != e {
		t.Errorf("approve_each over a hold: applied %v replaced %q rules %+v", applied, replaced, out)
	}
	if _, _, applied := narrowToolRule([]types.ToolRule{{Tool: approveEachTool, Effect: d}}, types.ToolRule{Tool: approveEachTool, Effect: e}); applied {
		t.Error("approve_each narrowed a deny")
	}
}

// "" is no opinion: a caller that starts from it and takes the stricter ends on the first real
// effect, so the shared order (which ranks an unknown effect with deny) never turns "" into a rule.
func TestStricterToolEffectNoOpinion(t *testing.T) {
	for _, e := range []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolApproveEach, types.ToolDeny} {
		if got := stricterToolEffect("", e); got != e {
			t.Errorf(`stricterToolEffect("", %q) = %q`, e, got)
		}
		if got := stricterToolEffect(e, ""); got != e {
			t.Errorf(`stricterToolEffect(%q, "") = %q`, e, got)
		}
	}
	if got := stricterToolEffect("", ""); got != "" {
		t.Errorf(`stricterToolEffect("", "") = %q`, got)
	}
	// compileHarnessRules starts every tool from "" when neither layer names it.
	got := compileHarnessRules(nil, types.ToolAllow, client.IncludedTool{ToolRules: []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}})
	if len(got) != 1 || got[0].Effect != types.ToolApproveEach {
		t.Errorf("a person's approve_each over the lane's allow: %+v", got)
	}
}

// A policy that asks for each call of a tool puts a background Claude Code run on the gated lane,
// as a hold or a block does, with "*: allow" under it.
func TestApplyToolPostureRoutesApproveEach(t *testing.T) {
	rule := types.ToolRule{Tool: approveEachTool, Effect: types.ToolApproveEach}
	req, spec := createRunRequest{Agent: claudeCodeHarness, Task: "t"}, types.RunPolicySpec{ToolRules: []types.ToolRule{rule}}
	(&Server{}).applyToolPosture(&req, &spec)
	want := []types.ToolRule{rule, {Tool: "*", Effect: types.ToolAllow}}
	if req.ToolApprovals != "hold" || !slices.Equal(spec.ToolRules, want) {
		t.Errorf("tool_approvals %q rules %v, want hold %v", req.ToolApprovals, spec.ToolRules, want)
	}
	if !macOSVMHolds(createRunRequest{Agent: claudeCodeHarness, Task: "t"}, types.RunPolicySpec{ToolRules: []types.ToolRule{rule}}) {
		t.Error("macOSVMHolds ignores approve_each, though applyToolPosture routes it to the hold lane")
	}
	// An interactive run keeps its lane: Claude Code's settings carry the rule.
	req, spec = createRunRequest{Agent: claudeCodeHarness, Interactive: true}, types.RunPolicySpec{ToolRules: []types.ToolRule{rule}}
	(&Server{}).applyToolPosture(&req, &spec)
	if req.ToolApprovals != "" || len(spec.ToolRules) != 1 {
		t.Errorf("interactive: tool_approvals %q rules %v", req.ToolApprovals, spec.ToolRules)
	}
}

// A ceiling's approve_each does not count in governanceHoldRules, so it is the tool posture that keeps a
// background run of a gated harness from running ungated: an explicit auto becomes hold.
func TestApplyToolPostureOverridesAutoForApproveEach(t *testing.T) {
	rule := types.ToolRule{Tool: approveEachTool, Effect: types.ToolApproveEach}
	req, spec := createRunRequest{Agent: claudeCodeHarness, Task: "t", ToolApprovals: "auto"}, types.RunPolicySpec{ToolRules: []types.ToolRule{rule}}
	(&Server{}).applyToolPosture(&req, &spec)
	if req.ToolApprovals != "hold" {
		t.Errorf("tool_approvals %q, want hold", req.ToolApprovals)
	}
}

// Wardyn can ask for each call only of a server it reaches through the proxy. A tool of a server
// that runs in the sandbox is refused for every harness, however the rule got onto the run.
func TestApproveEachOnLocalMCPRefused(t *testing.T) {
	local := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio}}
	remote := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.files.test/mcp"}}
	each := func(tool string) []types.ToolRule {
		return []types.ToolRule{{Tool: tool, Effect: types.ToolApproveEach}}
	}
	const sentence = "Wardyn can ask for each call only of an MCP server it reaches through the proxy; files runs in the sandbox."
	for _, tc := range []struct {
		name     string
		def      types.ComponentDefinition
		harness  string
		taskMode string
		policy   []types.ToolRule
		own      client.IncludedTool
		refused  bool
	}{
		{"claude, policy rule", local, claudeCodeHarness, "", each("mcp__files__write"), client.IncludedTool{}, true},
		{"codex, policy rule", local, codexHarness, "", each("mcp__files__write"), client.IncludedTool{}, true},
		{"claude in a command", local, claudeCodeHarness, "exec", each("mcp__files__write"), client.IncludedTool{}, true},
		{"claude, the person's own rule", local, claudeCodeHarness, "", nil,
			client.IncludedTool{ID: claudeCodeHarness, Kind: client.IncludedToolHarness, ToolRules: each("mcp__files__read")}, true},
		{"another server's tool", local, claudeCodeHarness, "", each("mcp__linear__write"), client.IncludedTool{}, false},
		{"a remote server", remote, claudeCodeHarness, "", each("mcp__files__write"), client.IncludedTool{}, false},
		{"a remote server, codex", remote, codexHarness, "", each("mcp__files__write"), client.IncludedTool{}, false},
	} {
		comps := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "Files", Definition: tc.def, MCPHarnesses: []string{tc.harness}}}}}
		req := createRunRequest{}
		req.TaskMode = tc.taskMode
		if tc.own.ID != "" {
			req.Tools = []client.IncludedTool{tc.own}
		}
		got := localMCPRulesRefusal(req, types.RunPolicySpec{ToolRules: tc.policy}, comps)
		if (got != nil) != tc.refused {
			t.Errorf("%s: refusal %+v, want refused=%v", tc.name, got, tc.refused)
			continue
		}
		if got != nil && (got.body.Reason != reasonRequestFieldUnavailable || got.body.Error != sentence) {
			t.Errorf("%s: %+v, want %q", tc.name, got.body, sentence)
		}
	}
}

// Codex CLI and a command-started harness take approve_each on a remote server's tool: the broker
// pauses the call, not the harness. A hold on the same tool is still refused.
func TestApproveEachHarnessWithoutGateNotRefused(t *testing.T) {
	each := types.ToolRule{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}
	hold := types.ToolRule{Tool: "mcp__linear__delete_issue", Effect: types.ToolHold}
	codex := func(r types.ToolRule) client.IncludedTool {
		return client.IncludedTool{ID: codexHarness, Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{r}}
	}
	if got := harnessToolRulesRefusal(codex(each)); got != nil {
		t.Errorf("Codex CLI's own approve_each on an MCP tool refused: %+v", got.body)
	}
	if got := harnessToolRulesRefusal(codex(hold)); got == nil {
		t.Error("Codex CLI's own hold accepted")
	}
	comps := remoteMCPComps(codexHarness)
	for name, req := range map[string]createRunRequest{
		"a background Codex CLI run": {Agent: codexHarness, Task: "t"},
		"a command":                  {Agent: codexHarness, TaskMode: "exec", Tools: []client.IncludedTool{{ID: codexHarness, Kind: client.IncludedToolHarness}}},
	} {
		if got := inheritedMCPHoldRefusal(req, types.RunPolicySpec{ToolRules: []types.ToolRule{each}}, comps); got != nil {
			t.Errorf("%s: policy approve_each refused: %+v", name, got.body)
		}
		if got := localMCPRulesRefusal(req, types.RunPolicySpec{ToolRules: []types.ToolRule{each}}, comps); got != nil {
			t.Errorf("%s: policy approve_each on a remote server refused: %+v", name, got.body)
		}
		if got := inheritedMCPHoldRefusal(req, types.RunPolicySpec{ToolRules: []types.ToolRule{hold}}, comps); got == nil {
			t.Errorf("%s: policy hold on the same tool accepted", name)
		}
	}
}

// Step 5 offers approve_each for a rule on a harness with a remote MCP server, never for the default,
// and says why not otherwise. The effects keep the order allow, hold, approve_each, deny.
func TestToolPolicyFactOffersApproveEach(t *testing.T) {
	option := func(opts []client.EffectOption, e types.ToolEffect) (client.EffectOption, bool) {
		i := slices.IndexFunc(opts, func(o client.EffectOption) bool { return o.Effect == e })
		if i < 0 {
			return client.EffectOption{}, false
		}
		return opts[i], true
	}
	background := createRunRequest{Agent: claudeCodeHarness, Task: "t", ToolApprovals: "hold"}
	for _, tc := range []struct {
		name         string
		harness      string
		req          createRunRequest
		comps        runComponents
		wantRule     bool
		wantRuleWhy  string
		wantDefaultW string
	}{
		{"claude, remote server", claudeCodeHarness, background, remoteMCPComps(claudeCodeHarness), true, "", "approve_each_default"},
		{"claude, no server", claudeCodeHarness, background, runComponents{}, false, "approve_each_no_remote_mcp", "approve_each_default"},
		{"claude, a server for codex only", claudeCodeHarness, background, remoteMCPComps(codexHarness), false, "approve_each_no_remote_mcp", "approve_each_default"},
		{"codex, remote server", codexHarness, createRunRequest{Agent: codexHarness, Task: "t"}, remoteMCPComps(codexHarness), true, "", "approve_each_default"},
		{"interactive claude, remote server", claudeCodeHarness, createRunRequest{Agent: claudeCodeHarness, Interactive: true}, remoteMCPComps(claudeCodeHarness), true, "", "approve_each_default"},
	} {
		req := tc.req
		f := (&accessBuilder{in: accessInputs{req: &req, comps: tc.comps}}).toolPolicyFact(tc.harness)
		var order []types.ToolEffect
		for _, o := range f.RuleEffects {
			order = append(order, o.Effect)
		}
		if want := []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolApproveEach, types.ToolDeny}; !slices.Equal(order, want) {
			t.Errorf("%s: rule effects %v, want %v", tc.name, order, want)
		}
		rule, ok := option(f.RuleEffects, types.ToolApproveEach)
		if !ok || rule.Available != tc.wantRule || rule.Reason != tc.wantRuleWhy {
			t.Errorf("%s: rule option %+v (offered %v), want available=%v reason %q", tc.name, rule, ok, tc.wantRule, tc.wantRuleWhy)
		}
		def, ok := option(f.Default.Options, types.ToolApproveEach)
		if !ok || def.Available || def.Reason != tc.wantDefaultW {
			t.Errorf("%s: default option %+v (offered %v), want unavailable with reason %q", tc.name, def, ok, tc.wantDefaultW)
		}
	}
	for _, code := range []string{"approve_each_default", "approve_each_no_remote_mcp"} {
		if accessSentences[code] == "" {
			t.Errorf("%s has no sentence", code)
		}
	}
	// A policy that asks for each call shows as a locked rule, and a person's allow is stricter-refused.
	req := background
	rules := []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}
	f := (&accessBuilder{in: accessInputs{req: &req, spec: types.RunPolicySpec{ToolRules: rules}, comps: remoteMCPComps(claudeCodeHarness)}}).toolPolicyFact(claudeCodeHarness)
	if len(f.PolicyRules) != 1 || f.PolicyRules[0].Effect != types.ToolApproveEach {
		t.Errorf("policy rules %+v", f.PolicyRules)
	}
}

// An interactive Claude Code run has no gate: its managed settings answer for each tool. A tool of a
// remote server turned on for Claude Code is allowed there, because the broker asks for each call;
// any other approve_each rule is a deny, since nothing could ask with the arguments.
func TestAttendedRulesMapApproveEach(t *testing.T) {
	each := func(tool string) types.ToolRule { return types.ToolRule{Tool: tool, Effect: types.ToolApproveEach} }
	comps := componentDispatch{MCP: []componentMCP{
		{Key: "linear", URL: "https://mcp.linear.test/mcp", Harnesses: []string{claudeCodeHarness}},
		{Key: "codexonly", URL: "https://mcp.codexonly.test/mcp", Harnesses: []string{codexHarness}},
		{Key: "files", Launch: &types.ComponentMCPLaunch{}, Harnesses: []string{claudeCodeHarness}},
	}}
	rules := []types.ToolRule{each("mcp__linear__delete_issue"), each("mcp__codexonly__x"), each("mcp__files__write"), each("mcp__nobody__x"),
		{Tool: "Bash", Effect: types.ToolHold}}
	want := []types.ToolRule{
		{Tool: "mcp__linear__delete_issue", Effect: types.ToolAllow}, {Tool: "mcp__codexonly__x", Effect: types.ToolDeny},
		{Tool: "mcp__files__write", Effect: types.ToolDeny}, {Tool: "mcp__nobody__x", Effect: types.ToolDeny},
		{Tool: "Bash", Effect: types.ToolHold}}
	got := dispatchParams{Interactive: true, Policy: types.RunPolicySpec{ToolRules: rules}, Components: comps}.attendedToolRules()
	if !slices.Equal(got, want) {
		t.Errorf("run-wide rules map to %v, want %v", got, want)
	}
	if err := agentpolicy.CheckToolRules(got); err != nil {
		t.Errorf("the mapped rules cannot be written into the managed settings: %v", err)
	}
	set := []types.HarnessToolRules{{Harness: claudeCodeHarness, Rules: rules[:1]}, {Harness: codexHarness, Rules: rules[:1]}}
	got = dispatchParams{Interactive: true, Policy: types.RunPolicySpec{ToolRules: rules}, HarnessToolRules: set, Components: comps}.attendedToolRules()
	if !slices.Equal(got, want[:1]) {
		t.Errorf("Claude Code's own set maps to %v, want %v", got, want[:1])
	}
	// The caller's slice is never rewritten: the same rules ride the proxy config for other lanes.
	if rules[0].Effect != types.ToolApproveEach {
		t.Error("attendedToolRules rewrote the policy's rules in place")
	}
	// The door that checks the names before dispatch accepts an approve_each rule on an MCP tool.
	interactive := createRunRequest{Agent: claudeCodeHarness, Interactive: true}
	if refusal := attendedToolNamesRefusal(interactive, types.RunPolicySpec{ToolRules: rules}); refusal != nil {
		t.Errorf("an interactive run with approve_each refused at create: %+v", refusal.body)
	}
	if got := (dispatchParams{Policy: types.RunPolicySpec{ToolRules: rules}, Components: comps}).attendedToolRules(); got != nil {
		t.Errorf("a background run has no attended rules: %v", got)
	}
}
