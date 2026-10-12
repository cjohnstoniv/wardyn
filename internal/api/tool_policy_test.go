// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// Until the harness's own settings carry them, an interactive run takes no rule
// and no default but its own (Claude Code asks you), refused by name rather
// than shown and never enforced.
func TestToolPolicy_AttendedRefusedUntilHarnessSettings(t *testing.T) {
	want := runModeUnavailableMsg("tools[0]: tool rules in an interactive run")
	for _, tool := range []client.IncludedTool{
		{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
		{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}},
		{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolDeny},
		{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolAllow},
	} {
		req := createRunRequest{Experience: client.ExperienceInteractive, Tools: []client.IncludedTool{tool}}
		if refusal := attendedToolRulesRefusal(req); refusal == nil || refusal.body.Reason != reasonRequestFieldUnavailable || refusal.body.Error != want {
			t.Errorf("%+v: refusal %+v, want %q", tool, refusal, want)
		}
		req.Experience = client.ExperienceBackground
		if refusal := attendedToolRulesRefusal(req); refusal != nil {
			t.Errorf("a background run's %+v refused: %+v", tool, refusal)
		}
	}
	asks := createRunRequest{Experience: client.ExperienceInteractive, Tools: []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolHold}}}
	if refusal := attendedToolRulesRefusal(asks); refusal != nil {
		t.Errorf("the lane's own default refused: %+v", refusal)
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
