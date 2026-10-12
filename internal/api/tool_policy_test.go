// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// toolCallSite is a run's tool gate as its harness meets it: the proxy sidecar
// booted from the config dispatch wrote, and whether the run is on the gated
// lane at all (the sandbox env dispatch composed).
type toolCallSite struct {
	url   string
	gated bool
}

// bootToolGate boots the real proxy from spec's own proxy config, with a control
// plane that answers a held call's approval as pending.
func bootToolGate(t *testing.T, spec runner.SandboxSpec) toolCallSite {
	t.Helper()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "approvals") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalToolCall, State: types.ApprovalPending})
	}))
	t.Cleanup(cp.Close)
	port := freeLoopbackPort(t)
	pc := spec.ProxyConfig
	pc.ControlPlaneURL = cp.URL
	raw, err := runner.BuildProxyConfig(spec.RunID, pc, port)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:" + strconv.Itoa(port)
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	go func() { _ = psrv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(ctx)
	})
	return toolCallSite{url: "http://" + cfg.Listen + "/wardyn/v1/approvals", gated: spec.Env["WARDYN_TOOL_APPROVALS"] == "hold"}
}

// call is one tool call as the harness makes it: off the gated lane nothing
// asks, so the tool runs (agent-run's --dangerously-skip-permissions); on it,
// wardyn-toolgate raises it at the proxy, with a Bearer only when the caller
// holds a credential. The outcome is "ran", "APPROVED", "DENIED" or "held".
func (c toolCallSite) call(t *testing.T, tool, bearer string) string {
	t.Helper()
	if !c.gated {
		return "ran"
	}
	var resp *http.Response
	var err error
	for range 50 {
		req, _ := http.NewRequest(http.MethodPost, c.url, strings.NewReader(`{"kind":"tool_call","payload":{"tool":"`+tool+`","cmd":"x"}}`))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("proxy unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusCreated:
		return "held"
	case resp.StatusCode == http.StatusOK && strings.Contains(string(body), "APPROVED"):
		return "APPROVED"
	case resp.StatusCode == http.StatusOK && strings.Contains(string(body), "DENIED"):
		return "DENIED"
	}
	t.Fatalf("%s: %d %s", tool, resp.StatusCode, body)
	return ""
}

// A person's allow is an exception to their own default and never reaches past
// the policy: under a policy that allows the tool it stands (and survives the
// person's own "block everything else"); under one that holds or blocks it, the
// fold refuses it by name, whether it is a rule, an explicit default or an
// override.
func TestToolPolicy_PersonalAllowNeverLoosensPolicy(t *testing.T) {
	personal := client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolDeny,
		ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}, {Tool: "Bash", Effect: types.ToolHold}}}
	posture := []types.ToolRule{{Tool: "WebFetch", Effect: types.ToolHold}, {Tool: "*", Effect: types.ToolAllow}}
	got := compileHarnessRules(posture, types.ToolHold, personal)
	for tool, want := range map[string]types.ToolEffect{"Read": types.ToolAllow, "Bash": types.ToolHold, "WebFetch": types.ToolDeny, "Other": types.ToolDeny} {
		if eff := effectiveToolEffect(got, tool); eff != want {
			t.Errorf("%s = %q, want %q (compiled %v)", tool, eff, want, got)
		}
	}
	// Under a policy that holds Read, the person's allow cannot lift it.
	if eff := effectiveToolEffect(compileHarnessRules([]types.ToolRule{{Tool: "Read", Effect: types.ToolHold}}, types.ToolHold, personal), "Read"); eff != types.ToolHold {
		t.Errorf("Read under a policy hold = %q, want hold", eff)
	}

	gated := createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold"}
	allowRead := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}}}
	allowDefault := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolAllow}}
	srv := &Server{}
	for _, tc := range []struct {
		name  string
		tools []client.IncludedTool
		agent *client.AgentOverrides
		rules []types.ToolRule
		msg   string
	}{
		{"a rule under a policy allow", allowRead, nil, []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}, ""},
		{"a rule under the posture's allow", allowRead, nil, posture, ""},
		{"a rule under a policy hold", allowRead, nil, []types.ToolRule{{Tool: "Read", Effect: types.ToolHold}, {Tool: "*", Effect: types.ToolAllow}},
			"Your policy asks first for Read. A run can only be stricter."},
		{"a rule under a policy block", allowRead, nil, []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}}, "Your policy blocks Read. A run can only be stricter."},
		{"a rule the policy does not name, on a lane that holds it", allowRead, nil, nil, "Your policy asks first for Read. A run can only be stricter."},
		{"a default under a policy allow", allowDefault, nil, posture, ""},
		{"a default on a lane that holds", allowDefault, nil, []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}},
			"Your policy asks first for tools with no rule. A run can only be stricter."},
		{"an override under a policy hold", nil, &client.AgentOverrides{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}}, nil,
			"Your policy asks first for Bash. A run can only be stricter."},
	} {
		req := gated
		req.Tools = tc.tools
		if tc.agent != nil {
			req.Overrides = &client.RunOverrides{Agent: tc.agent}
		}
		refusal := srv.toolRulesWithinPolicyRefusal(req, types.RunPolicySpec{ToolRules: tc.rules})
		switch {
		case tc.msg == "" && refusal != nil:
			t.Errorf("%s: refused %+v", tc.name, refusal)
		case tc.msg != "" && (refusal == nil || refusal.body.Reason != reasonOverrideRefused || refusal.body.Error != tc.msg):
			t.Errorf("%s: refusal %+v, want %s %q", tc.name, refusal, reasonOverrideRefused, tc.msg)
		}
	}
	// An override's allow is never laid onto the spec: within the policy it is
	// already there, and past it the fold refuses it.
	spec := types.RunPolicySpec{}
	applyToolRuleOverrides(&spec, []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}, &overrideEdits{})
	if len(spec.ToolRules) != 0 {
		t.Errorf("an override allow was applied: %v", spec.ToolRules)
	}
	if row, _ := types.OverrideRuleFor(types.OverrideToolRuleAllow, types.OverrideAdd); row.Rule != types.OverrideWithinPolicy {
		t.Errorf("tool_rule_allow add = %s, want within_policy", row.Rule)
	}
}

// Codex CLI cannot pause a call and Wardyn sees none of its own tools, so a hold
// or a block on them is refused by name; a block on its MCP tool compiles into
// its rule set, where the MCP route reads it.
func TestToolPolicy_CodexBuiltinRuleRefused(t *testing.T) {
	for _, tc := range []struct {
		tool client.IncludedTool
		msg  string
	}{
		{client.IncludedTool{ID: "codex-cli", ToolRules: []types.ToolRule{{Tool: "shell", Effect: types.ToolDeny}}}, codexBuiltinRuleMsg("shell")},
		{client.IncludedTool{ID: "codex-cli", ToolRules: []types.ToolRule{{Tool: "apply_patch", Effect: types.ToolAllow}}}, codexBuiltinRuleMsg("apply_patch")},
		{client.IncludedTool{ID: "codex-cli", DefaultEffect: types.ToolDeny}, accessSentences["harness_no_block_builtin"]},
		{client.IncludedTool{ID: "codex-cli", DefaultEffect: types.ToolHold}, accessSentences["harness_no_hold"]},
		{client.IncludedTool{ID: "codex-cli", ToolRules: []types.ToolRule{{Tool: "mcp__github__create_issue", Effect: types.ToolHold}}}, accessSentences["harness_no_hold"]},
		{client.IncludedTool{ID: "none", ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}}, "Wardyn can't set tool rules for Your own tools."},
		{client.IncludedTool{ID: "my-harness", DefaultEffect: types.ToolDeny}, "Wardyn can't set tool rules for my-harness."},
	} {
		refusal := harnessToolRulesRefusal(tc.tool)
		if refusal == nil || refusal.body.Reason != reasonRequestFieldUnavailable || refusal.body.Error != tc.msg {
			t.Errorf("%+v: refusal %+v, want %q", tc.tool, refusal, tc.msg)
		}
	}
	for _, ok := range []client.IncludedTool{
		{ID: "codex-cli", ToolRules: []types.ToolRule{{Tool: "mcp__github__create_issue", Effect: types.ToolDeny}}},
		{ID: "codex-cli", DefaultEffect: types.ToolAllow},
		{ID: "codex-cli"},
		{ID: "claude-code", DefaultEffect: types.ToolHold, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
	} {
		if refusal := harnessToolRulesRefusal(ok); refusal != nil {
			t.Errorf("%+v refused: %+v", ok, refusal)
		}
	}
}

// A Codex CLI block on its MCP tool reaches the rule set the run carries, beside
// the policy's rules, and the run stays off the gated lane Codex cannot honour.
func TestToolPolicy_CodexMCPRuleCompiled(t *testing.T) {
	codex := client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness,
		ToolRules: []types.ToolRule{{Tool: "mcp__github__create_issue", Effect: types.ToolDeny}, {Tool: "mcp__github__list_issues", Effect: types.ToolAllow}}}
	req := createRunRequest{Agent: "codex-cli", Task: "t", Tools: []client.IncludedTool{codex}}
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}}
	(&Server{}).applyToolPosture(&req, &spec)
	if req.ToolApprovals != "" {
		t.Fatalf("a Codex run went on the gated lane: %q", req.ToolApprovals)
	}
	if refusal := (&Server{}).toolRulesWithinPolicyRefusal(req, spec); refusal != nil {
		t.Fatalf("an MCP allow on Codex's ungated lane was refused: %+v", refusal)
	}
	out, sets := applyHarnessToolRules(spec, req.Tools, toolLaneDefault(req))
	if sets != nil {
		t.Fatalf("one harness carried per-harness sets: %+v", sets)
	}
	for tool, want := range map[string]types.ToolEffect{"mcp__github__create_issue": types.ToolDeny, "mcp__github__list_issues": types.ToolAllow, "Read": types.ToolAllow} {
		if eff := effectiveToolEffect(out.ToolRules, tool); eff != want {
			t.Errorf("%s = %q, want %q (rules %v)", tool, eff, want, out.ToolRules)
		}
	}
}

// In an interactive run Claude Code takes every rule (its managed settings
// carry them), and no other harness takes any; Claude Code's default is held,
// in the fold, to what its settings can say at the run's level, with the
// sentence step 5 greys it with: "Run it" can never skip asking where the
// level's document locks bypass off (L0, L1), and neither "Run it" nor
// "Block it" has a setting anywhere else.
func TestToolPolicy_AttendedRulesGoToClaudeCodeSettings(t *testing.T) {
	interactive := func(tool client.IncludedTool) createRunRequest {
		return createRunRequest{Experience: client.ExperienceInteractive, Interactive: true, Tools: []client.IncludedTool{tool}}
	}
	claude := client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness}
	for _, rules := range [][]types.ToolRule{{{Tool: "Bash", Effect: types.ToolDeny}}, {{Tool: "Bash", Effect: types.ToolHold}}, {{Tool: "Read", Effect: types.ToolAllow}}} {
		tool := claude
		tool.ToolRules = rules
		if refusal := attendedToolRulesRefusal(interactive(tool)); refusal != nil {
			t.Errorf("Claude Code rules %v refused at decode: %+v", rules, refusal)
		}
		codex := tool
		codex.ID = "codex-cli"
		if refusal := attendedToolRulesRefusal(interactive(codex)); refusal == nil || refusal.body.Error != runModeUnavailableMsg("tools[0]: tool rules in an interactive run") {
			t.Errorf("Codex CLI rules %v in an interactive run: %+v, want refused", rules, refusal)
		}
	}
	for _, c := range []struct {
		level   types.AutonomyLevel
		effect  types.ToolEffect
		refused string
	}{
		{types.AutonomyL0, types.ToolAllow, "attended_no_skip"},
		{types.AutonomyL1, types.ToolAllow, "attended_no_skip"},
		{types.AutonomyL2, types.ToolAllow, "attended_rules_unavailable"},
		{types.AutonomyL3, types.ToolAllow, "attended_rules_unavailable"},
		{"", types.ToolAllow, "attended_rules_unavailable"},
		{types.AutonomyL1, types.ToolDeny, "attended_rules_unavailable"},
		{"", types.ToolDeny, "attended_rules_unavailable"},
		{types.AutonomyL1, types.ToolHold, ""},
		{"", "", ""},
	} {
		tool := claude
		tool.DefaultEffect = c.effect
		req := interactive(tool)
		refusal := attendedDefaultRefusal(req, c.level)
		if c.refused == "" {
			if refusal != nil {
				t.Errorf("%s default %q refused: %+v", c.level, c.effect, refusal)
			}
			continue
		}
		if refusal == nil || refusal.body.Reason != reasonRequestFieldUnavailable || refusal.body.Error != accessSentences[c.refused] {
			t.Errorf("%s default %q: %+v, want refused with %s", c.level, c.effect, refusal, c.refused)
		}
		// The fact greys the same default with the same code.
		f := (&accessBuilder{in: accessInputs{req: &req, level: c.level}}).toolPolicyFact("claude-code")
		for _, o := range f.Default.Options {
			if o.Effect == c.effect && (o.Available || o.Reason != c.refused) {
				t.Errorf("%s default %q offered %+v, refused %q", c.level, c.effect, o, c.refused)
			}
		}
		req.Interactive, req.Experience, req.Task = false, client.ExperienceBackground, "t"
		if attendedDefaultRefusal(req, c.level) != nil {
			t.Errorf("a background run's default %q refused as an interactive one's", c.effect)
		}
	}
}

// A harness started by hand beside the one Wardyn starts holds no credential (it
// is delivered to that one launch alone), so its calls are not the started
// harness's and take the strictest of the sets; and on the gated lane the
// person's deny is still the started harness's answer. Both are tool calls
// through the proxy dispatch configured.
func TestToolPolicy_HandStartedHarnessHasNoCredentialAndDenyHolds(t *testing.T) {
	req := createRunRequest{Agent: "claude-code", Task: "t", Tools: []client.IncludedTool{
		{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "WebFetch", Effect: types.ToolDeny}}},
		{ID: "codex-cli", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "mcp__github__create_issue", Effect: types.ToolDeny}}},
	}}
	spec := types.RunPolicySpec{}
	srv0 := &Server{}
	srv0.applyToolPosture(&req, &spec)
	if refusal := srv0.toolRulesWithinPolicyRefusal(req, spec); refusal != nil {
		t.Fatal(refusal.body.Error)
	}
	spec, sets := applyHarnessToolRules(spec, req.Tools, toolLaneDefault(req))
	fr := &fakeRunner{}
	srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "img", ToolApprovals: req.ToolApprovals, Policy: spec, HarnessToolRules: sets,
	})
	if fr.createCalls != 1 {
		t.Fatal("the sandbox was not created")
	}
	sb := fr.lastSpec
	started := ""
	for _, f := range sb.ManagedFiles {
		if f.Path == runner.ComponentSecretDir+"/"+toolgateCredentialFilePrefix+"claude-code" {
			started = string(f.Content)
		}
	}
	if started == "" {
		t.Fatal("the started harness was given no credential")
	}
	// A process started by hand inherits the container's environment, secret or not.
	hand := sb.SecretEnv["WARDYN_TOOLGATE_CREDENTIAL"]
	if v := sb.Env["WARDYN_TOOLGATE_CREDENTIAL"]; v != "" {
		hand = v
	}
	gate := bootToolGate(t, sb)
	// (i) The hand-started process presents nothing, and its call is not the
	// started harness's: claude-code allows the Codex MCP tool, the strictest set
	// does not.
	if hand != "" {
		t.Error("a hand-started process inherits a toolgate credential from the container's environment")
	}
	if got := gate.call(t, "mcp__github__create_issue", started); got != "APPROVED" {
		t.Errorf("the started harness's own allow: %s", got)
	}
	if got := gate.call(t, "mcp__github__create_issue", hand); got == "APPROVED" || got == "ran" {
		t.Errorf("a hand-started call was answered as the started harness's: %s", got)
	}
	// (ii) The person's deny holds on the gated lane, for the started harness and
	// for one started by hand.
	if got := gate.call(t, "WebFetch", started); got != "DENIED" {
		t.Errorf("the started harness's denied tool: %s, want DENIED", got)
	}
	if got := gate.call(t, "WebFetch", hand); got != "DENIED" && got != "held" {
		t.Errorf("a hand-started harness's denied tool: %s, want held or DENIED", got)
	}
	if got := gate.call(t, "Read", started); got != "APPROVED" {
		t.Errorf("a tool nobody names: %s, want the posture's allow", got)
	}
}

// TestToolEffectGolden writes the cases the console's twin of policyToolEffect
// (ui/src/app/lib/tool-effect.ts) is pinned to; WARDYN_UPDATE_GOLDEN=1 rewrites it.
func TestToolEffectGolden(t *testing.T) {
	type tc struct {
		Name        string           `json:"name"`
		PolicyRules []types.ToolRule `json:"policy_rules"`
		LaneDefault types.ToolEffect `json:"lane_default"`
		Tool        string           `json:"tool"`
		Effect      types.ToolEffect `json:"effect"`
	}
	hold, allow, deny := types.ToolHold, types.ToolAllow, types.ToolDeny
	cases := []tc{
		{Name: "no rule on the gated lane is held", PolicyRules: []types.ToolRule{}, LaneDefault: hold, Tool: "Bash"},
		{Name: "no rule on an ungated lane runs", PolicyRules: []types.ToolRule{}, LaneDefault: allow, Tool: "Bash"},
		{Name: "the exact rule wins over the default", PolicyRules: []types.ToolRule{{Tool: "*", Effect: deny}, {Tool: "Read", Effect: allow}}, LaneDefault: hold, Tool: "Read"},
		{Name: "the policy's default wins over the lane's", PolicyRules: []types.ToolRule{{Tool: "Read", Effect: allow}, {Tool: "*", Effect: deny}}, LaneDefault: allow, Tool: "Bash"},
		{Name: "the posture's allow under the person's layer", PolicyRules: []types.ToolRule{{Tool: "WebFetch", Effect: hold}, {Tool: "*", Effect: allow}}, LaneDefault: hold, Tool: "Bash"},
		{Name: "a held tool under the posture", PolicyRules: []types.ToolRule{{Tool: "WebFetch", Effect: hold}, {Tool: "*", Effect: allow}}, LaneDefault: hold, Tool: "WebFetch"},
		{Name: "names match exactly and by case", PolicyRules: []types.ToolRule{{Tool: "bash", Effect: allow}}, LaneDefault: hold, Tool: "Bash"},
		{Name: "an MCP tool by its full name", PolicyRules: []types.ToolRule{{Tool: "mcp__github__create_issue", Effect: deny}}, LaneDefault: allow, Tool: "mcp__github__create_issue"},
		{Name: "the default itself", PolicyRules: []types.ToolRule{{Tool: "*", Effect: hold}}, LaneDefault: allow, Tool: "*"},
	}
	for i := range cases {
		cases[i].Effect = policyToolEffect(cases[i].PolicyRules, cases[i].LaneDefault, cases[i].Tool)
	}
	compareOrUpdateGolden(t, "../../ui/src/app/lib/tool-effect.golden.json", cases)
	if cases[0].Effect != hold || cases[1].Effect != allow || cases[4].Effect != allow || cases[6].Effect != hold {
		t.Errorf("policyToolEffect: %+v", cases)
	}
}

// Each harness's tool facts offer exactly what the server accepts: the lane and
// its enforcer, the default with its source, and every effect greyed with the
// reason the server would refuse it.
func TestToolPolicy_HarnessFactsOfferWhatTheServerAccepts(t *testing.T) {
	avail := func(opts []client.EffectOption) map[types.ToolEffect]string {
		out := map[types.ToolEffect]string{}
		for _, o := range opts {
			if !o.Available {
				out[o.Effect] = o.Reason
			}
		}
		return out
	}
	claude := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "WebFetch", Effect: types.ToolDeny}}}}
	for _, tc := range []struct {
		name, harness           string
		req                     createRunRequest
		spec                    []types.ToolRule
		prov                    []provenanceRow
		lane, enforcer, ruleSet string
		effective               types.ToolEffect
		source                  string
		greyDefault, greyRule   map[types.ToolEffect]string
	}{
		{"a person's rule on a background run", "claude-code", createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold", Tools: claude},
			[]types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}, []provenanceRow{{Field: fieldToolRules, Value: "*:allow", Source: provSource{Kind: provKindPosture}, Effect: provEffectAdded}},
			toolLaneGated, toolEnforcerGate, "any", types.ToolAllow, provKindPosture, map[types.ToolEffect]string{}, map[types.ToolEffect]string{}},
		{"a profile's gated lane holds what the policy does not name", "claude-code", createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold"},
			[]types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, nil, toolLaneGated, toolEnforcerGate, "any", types.ToolHold, provKindHarness,
			map[types.ToolEffect]string{types.ToolAllow: "policy_stricter_default"}, map[types.ToolEffect]string{types.ToolAllow: "policy_stricter_tool"}},
		{"an ungated run", "claude-code", createRunRequest{Agent: "claude-code", Task: "t"}, nil, nil, toolLaneUngated, toolEnforcerNone, "any", types.ToolAllow, provKindHarness,
			map[types.ToolEffect]string{}, map[types.ToolEffect]string{}},
		{"Codex CLI", "codex-cli", createRunRequest{Agent: "codex-cli", Task: "t"}, nil, nil, toolLaneUngated, toolEnforcerNone, "mcp_only", types.ToolAllow, provKindHarness,
			map[types.ToolEffect]string{types.ToolHold: "harness_no_hold", types.ToolDeny: "harness_no_block_builtin"}, map[types.ToolEffect]string{types.ToolHold: "harness_no_hold"}},
		{"an interactive run", "claude-code", createRunRequest{Agent: "claude-code", Interactive: true}, nil, nil, toolLaneAttended, toolEnforcerHarness, "any", types.ToolHold, provKindPosture,
			map[types.ToolEffect]string{types.ToolAllow: "attended_rules_unavailable", types.ToolDeny: "attended_rules_unavailable"},
			map[types.ToolEffect]string{types.ToolAllow: "policy_stricter_tool"}},
		// Claude Code's settings carry no "*", so whatever the policy's, an unnamed
		// tool takes the lane's own default; at no level no document manages it.
		{"an interactive run whose policy allows every tool", "claude-code", createRunRequest{Agent: "claude-code", Interactive: true},
			[]types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}, nil, toolLaneAttended, toolEnforcerHarness, "any", types.ToolHold, provKindPosture,
			map[types.ToolEffect]string{types.ToolAllow: "attended_rules_unavailable", types.ToolDeny: "attended_rules_unavailable"},
			map[types.ToolEffect]string{}},
		{"an interactive run whose policy blocks every tool", "claude-code", createRunRequest{Agent: "claude-code", Interactive: true},
			[]types.ToolRule{{Tool: "*", Effect: types.ToolDeny}}, nil, toolLaneAttended, toolEnforcerHarness, "any", types.ToolHold, provKindPosture,
			map[types.ToolEffect]string{types.ToolAllow: "attended_rules_unavailable", types.ToolDeny: "attended_rules_unavailable"},
			map[types.ToolEffect]string{types.ToolAllow: "policy_stricter_tool", types.ToolHold: "policy_stricter_tool"}},
		{"your own tools", "none", createRunRequest{Agent: "none", Interactive: true}, nil, nil, toolLaneUnsupported, toolEnforcerNone, "any", types.ToolHold, provKindHarness,
			map[types.ToolEffect]string{types.ToolAllow: "custom_harness_rules", types.ToolHold: "custom_harness_rules", types.ToolDeny: "custom_harness_rules"},
			map[types.ToolEffect]string{types.ToolAllow: "custom_harness_rules", types.ToolHold: "custom_harness_rules", types.ToolDeny: "custom_harness_rules"}},
	} {
		req := tc.req
		b := &accessBuilder{in: accessInputs{req: &req, spec: types.RunPolicySpec{ToolRules: tc.spec}, prov: tc.prov}}
		f := b.toolPolicyFact(tc.harness)
		if f.Lane != tc.lane || f.Enforcer != tc.enforcer || f.RuleTools != tc.ruleSet || f.Default.Effective != tc.effective || f.Default.Source.Kind != tc.source {
			t.Errorf("%s: %+v", tc.name, f)
		}
		if got := avail(f.Default.Options); !maps.Equal(got, tc.greyDefault) {
			t.Errorf("%s: default greyed %v, want %v", tc.name, got, tc.greyDefault)
		}
		if got := avail(f.RuleEffects); !maps.Equal(got, tc.greyRule) {
			t.Errorf("%s: rule effects greyed %v, want %v", tc.name, got, tc.greyRule)
		}
		locked := slices.DeleteFunc(slices.Clone(tc.spec), func(r types.ToolRule) bool { return tc.lane == toolLaneAttended && r.Tool == "*" })
		if len(f.PolicyRules) != len(locked) || f.MCPServers == nil {
			t.Errorf("%s: policy rules %v, mcp servers %v", tc.name, f.PolicyRules, f.MCPServers)
		}
		for code := range maps.Values(tc.greyRule) {
			if accessSentences[code] == "" {
				t.Errorf("%s: %s has no sentence", tc.name, code)
			}
		}
		// The server refuses the greyed allow exactly where the facts grey it.
		allowRead := req
		allowRead.Tools = []client.IncludedTool{{ID: tc.harness, Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}}}
		refused := (&Server{}).toolRulesWithinPolicyRefusal(allowRead, types.RunPolicySpec{ToolRules: tc.spec}) != nil ||
			harnessToolRulesRefusal(allowRead.Tools[0]) != nil || (allowRead.Interactive && attendedToolRulesRefusal(createRunRequest{Experience: client.ExperienceInteractive, Tools: allowRead.Tools}) != nil)
		if _, greyed := tc.greyRule[types.ToolAllow]; refused != greyed && tc.harness != "codex-cli" {
			t.Errorf("%s: a rule allow refused %v, greyed %v", tc.name, refused, greyed)
		}
	}
	// An interactive run's default, level by level: who answers a tool no rule
	// names. L0 and L1's settings ask; L2's (locked or not, the same mode) accept
	// file edits unasked; with no level or L3 no document manages it.
	for _, c := range []struct {
		level types.AutonomyLevel
		src   client.ProvenanceSource
		skip  string
	}{
		{types.AutonomyL0, client.ProvenanceSource{Kind: provKindHarness}, "attended_no_skip"},
		{types.AutonomyL1, client.ProvenanceSource{Kind: provKindHarness}, "attended_no_skip"},
		{types.AutonomyL2, client.ProvenanceSource{Kind: provKindHarness, ID: attendedSettingsAcceptEdits}, "attended_rules_unavailable"},
		{types.AutonomyL3, client.ProvenanceSource{Kind: provKindPosture}, "attended_rules_unavailable"},
		{"", client.ProvenanceSource{Kind: provKindPosture}, "attended_rules_unavailable"},
	} {
		req := createRunRequest{Agent: "claude-code", Interactive: true}
		f := (&accessBuilder{in: accessInputs{req: &req, level: c.level, spec: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}, {Tool: "Bash", Effect: types.ToolDeny}}}}}).toolPolicyFact("claude-code")
		if f.Default.Effective != types.ToolHold || f.Default.Source != c.src || f.Enforcer != toolEnforcerHarness {
			t.Errorf("%q: default %+v enforcer %s, want hold from %+v", c.level, f.Default, f.Enforcer, c.src)
		}
		if got := avail(f.Default.Options); !maps.Equal(got, map[types.ToolEffect]string{types.ToolAllow: c.skip, types.ToolDeny: "attended_rules_unavailable"}) {
			t.Errorf("%q: default greyed %v", c.level, got)
		}
		if len(f.PolicyRules) != 1 || f.PolicyRules[0].Tool != "Bash" {
			t.Errorf("%q: locked rows %+v, want Bash alone: the policy's \"*\" is not written to Claude Code's settings", c.level, f.PolicyRules)
		}
	}
	// A person's stricter default is theirs.
	req := createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold",
		Tools: []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolDeny}}}
	f := (&accessBuilder{in: accessInputs{req: &req, spec: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}}}}).toolPolicyFact("claude-code")
	if f.Default.Effective != types.ToolDeny || f.Default.Source.Kind != provKindPerson {
		t.Errorf("a person's default block: %+v", f.Default)
	}
}
