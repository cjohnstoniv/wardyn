// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func mcpInline(name string) types.ComponentRef {
	return types.ComponentRef{Name: name, Inline: &types.ComponentDefinition{
		Hosts: []string{"mcp.linear.test", "api.linear.test"},
		MCP:   &types.ComponentMCP{URL: "https://mcp.linear.test/mcp", Transport: types.ComponentMCPTransportHTTP},
		Secrets: []types.ComponentSecret{{SecretName: compOwnSecret,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "mcp.linear.test"}}},
	}}
}

func harnessTools(ids ...string) []client.IncludedTool {
	var out []client.IncludedTool
	for _, id := range ids {
		out = append(out, client.IncludedTool{ID: id, Kind: client.IncludedToolHarness})
	}
	return out
}

// TestRunComponents_MCPServerIsBrokerOnly: the gate admits an MCP component
// without putting its server's host on the run's allowlist (its other hosts
// still are), keeps its credential off interception, turns it on for the
// harnesses that can take it, and records them on the snapshot and the audit.
func TestRunComponents_MCPServerIsBrokerOnly(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	req := createRunRequest{Components: []types.ComponentRef{mcpInline("Linear")}}
	req.Tools = harnessTools("claude-code", "codex-cli", "my-harness")
	spec := govDeployment()
	comps, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal.body)
	}
	if slices.Contains(spec.AllowedDomains, "mcp.linear.test") || !slices.Contains(spec.AllowedDomains, "api.linear.test") {
		t.Errorf("allowlist %v: want api.linear.test and never the MCP server's host", spec.AllowedDomains)
	}
	if len(comps.mitmHosts) != 0 || !slices.Equal(comps.mcpHosts, []string{"mcp.linear.test"}) {
		t.Errorf("mitmHosts %v mcpHosts %v", comps.mitmHosts, comps.mcpHosts)
	}
	if got := comps.attached[0].snapshot.MCPHarnesses; !slices.Equal(got, []string{"claude-code", "codex-cli"}) {
		t.Errorf("default harnesses %v, want every one that can take it", got)
	}
	if e := comps.attached[0].auditEntry(0); !slices.Equal(e["mcp_harnesses"].([]string), []string{"claude-code", "codex-cli"}) {
		t.Errorf("audit entry %v", e)
	}
	d := comps.dispatch()
	if len(d.MITMHosts) != 0 || !slices.Equal(d.HeaderHosts, []string{"mcp.linear.test"}) || len(d.MCP) != 1 || d.MCP[0].Key != "linear" {
		t.Errorf("dispatch %+v", d)
	}

	// Chosen harnesses: one the run lacks, one that cannot take it.
	for ask, want := range map[string]string{"zed": reasonComponentRefInvalid, "my-harness": reasonRequestFieldUnavailable} {
		ref := mcpInline("Linear")
		ref.MCPHarnesses = []string{ask}
		req.Components = []types.ComponentRef{ref}
		spec = govDeployment()
		_, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
		if refusal == nil || refusal.body.Reason != want || !strings.Contains(refusal.body.Error, "components[0].mcp_harnesses[0]") {
			t.Errorf("%s: refusal %+v, want %s", ask, refusal, want)
		}
	}
	// A component without a server takes no harnesses.
	plain := types.ComponentRef{Inline: &types.ComponentDefinition{Hosts: []string{"svc.example"}}, MCPHarnesses: []string{"claude-code"}}
	req.Components, spec = []types.ComponentRef{plain}, govDeployment()
	if _, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true); refusal == nil ||
		refusal.body.Reason != reasonComponentRefInvalid {
		t.Errorf("plain component with harnesses: %+v", refusal)
	}
}

func TestMCPServerKeys(t *testing.T) {
	mk := func(name string, mcp bool) attachedComponent {
		a := attachedComponent{snapshot: types.RunComponent{Name: name}}
		if mcp {
			a.snapshot.Definition.MCP = &types.ComponentMCP{URL: "https://x.test/mcp"}
		}
		return a
	}
	got := mcpServerKeys([]attachedComponent{mk("Linear", true), mk("API", false), mk("linear!", true), mk("", true),
		mk("Über __ Tool", true), mk(strings.Repeat("a", 40), true)})
	want := []string{"linear", "", "linear-2", "mcp", "ber-tool", strings.Repeat("a", 28)}
	if !slices.Equal(got, want) {
		t.Fatalf("keys %q, want %q", got, want)
	}
}

// TestDispatchMCP_CredentialMovesToBroker: dispatch takes each MCP server's
// header credential off the generic injector's list and onto the server's
// broker entry, and gives each harness its own token, the proxy only the
// digest. A server whose credential a phase withheld is not brokered.
func TestDispatchMCP_CredentialMovesToBroker(t *testing.T) {
	grant, other := uuid.New(), uuid.New()
	injections := []runner.InjectionGrant{
		{GrantID: other, Rule: egress.InjectionRule{Host: "api.other.test"}},
		{GrantID: grant, Rule: egress.InjectionRule{Host: "MCP.linear.test"}},
	}
	servers := []componentMCP{
		{Ordinal: 0, Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{"claude-code", "codex-cli"}, Credentialed: true},
		{Ordinal: 1, Key: "gone", URL: "https://mcp.gone.test/mcp", Host: "mcp.gone.test", Harnesses: []string{"claude-code"}, Credentialed: true},
		{Ordinal: 2, Key: "public", URL: "https://mcp.public.test/mcp", Host: "mcp.public.test", Harnesses: []string{"claude-code"}},
	}
	env := map[string]string{}
	out := dispatchMCP(servers, &injections, "http://wardyn-proxy:3128/", false, env)
	if len(injections) != 1 || injections[0].GrantID != other {
		t.Fatalf("injections %+v: the MCP credential must leave the generic injector", injections)
	}
	if len(out.servers) != 2 || out.servers[0].GrantID == nil || *out.servers[0].GrantID != grant || out.servers[1].Key != "public" || out.servers[1].GrantID != nil {
		t.Fatalf("servers %+v", out.servers)
	}
	cfg := proxy.Config{MCPServers: out.servers, MCPBindings: out.bindings}
	urls := map[string]string{}
	if err := json.Unmarshal([]byte(env[codexMCPEnv]), &urls); err != nil || len(urls) != 1 {
		t.Fatalf("codex env %q: %v", env[codexMCPEnv], err)
	}
	if len(out.files) != 1 || out.files[0].Path != claudeMCPAutonomousPath {
		t.Fatalf("files %+v", out.files)
	}
	var claude struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.files[0].Content, &claude); err != nil || len(claude.MCPServers) != 2 || claude.MCPServers["public"].Type != "http" {
		t.Fatalf("claude file %s: %v", out.files[0].Content, err)
	}
	// Each URL's token is its harness's, known to the proxy by digest alone.
	for harness, u := range map[string]string{"codex-cli": urls["linear"], "claude-code": claude.MCPServers["linear"].URL} {
		tok, ok := strings.CutPrefix(u, "http://wardyn-proxy:3128/wardyn/mcp/")
		tok, _, _ = strings.Cut(tok, "/")
		sum := sha256.Sum256([]byte(tok))
		i := slices.IndexFunc(cfg.MCPBindings, func(b proxy.MCPBinding) bool { return b.Harness == harness })
		if !ok || i < 0 || cfg.MCPBindings[i].SHA256 != hex.EncodeToString(sum[:]) || strings.Contains(string(mustJSON(cfg)), tok) {
			t.Fatalf("%s: url %q, bindings %+v", harness, u, cfg.MCPBindings)
		}
	}
	if out := dispatchMCP(servers[2:], &injections, "http://p:3128", true, env); out.files[0].Path != claudeMCPInteractivePath {
		t.Fatalf("interactive file %s", out.files[0].Path)
	}
}

func TestReviveMCPBindingsNarrows(t *testing.T) {
	cfg := proxy.Config{
		MCPServers:  []proxy.MCPServerConfig{{Key: "linear", Ordinal: 0}, {Key: "erased", Ordinal: 1}},
		MCPBindings: []proxy.MCPBinding{{Harness: "claude-code", Servers: []string{"linear", "erased"}}, {Harness: "codex-cli", Servers: []string{"linear"}}},
	}
	mcp := types.ComponentDefinition{MCP: &types.ComponentMCP{}}
	rows := []types.RunComponent{{Ordinal: 0, Definition: mcp, MCPHarnesses: []string{"claude-code"}}, {Ordinal: 1, Erased: true, MCPHarnesses: []string{"claude-code"}}}
	narrowMCPBindings(&cfg, rows)
	if len(cfg.MCPServers) != 1 || !slices.Equal(cfg.MCPBindings[0].Servers, []string{"linear"}) || len(cfg.MCPBindings[1].Servers) != 0 {
		t.Fatalf("narrowed %+v", cfg)
	}
}

// TestAccessFacts_MCPRowAndBrokerEnforcer: step 5 shows an MCP component as an
// mcp row whose server's host is reached via the broker, each harness's
// servers, and the broker as the enforcer of Codex CLI's (MCP-only) rules.
func TestAccessFacts_MCPRowAndBrokerEnforcer(t *testing.T) {
	def := *mcpInline("Linear").Inline
	comps := runComponents{attached: []attachedComponent{{source: componentSourceInline,
		snapshot: types.RunComponent{SelfDefined: true, Name: "Linear", Definition: def, MCPHarnesses: []string{"codex-cli"}}}}}
	req := createRunRequest{}
	req.Tools = harnessTools("claude-code", "codex-cli")
	b := &accessBuilder{in: accessInputs{req: &req, comps: comps}, hosts: map[string]*client.AccessHost{}}
	b.componentRow(componentFact{ID: "inline:0", Name: "Linear", Status: componentReady, Hosts: def.Hosts})
	row := b.out.Rows[0]
	if row.Kind != accessKindMCP || row.MCP == nil || row.MCP.ServerKey != "linear" || !slices.Equal(row.MCP.Harnesses, []string{"codex-cli"}) {
		t.Fatalf("row %+v", row)
	}
	if b.hosts["mcp.linear.test"].Via != viaMCPBroker || b.hosts["api.linear.test"].Via != viaEgress {
		t.Fatalf("hosts %+v %+v", b.hosts["mcp.linear.test"], b.hosts["api.linear.test"])
	}
	codex, claude := b.toolPolicyFact("codex-cli"), b.toolPolicyFact("claude-code")
	if codex.Enforcer != toolEnforcerMCPBroker || !slices.Equal(codex.MCPServers, []string{"linear"}) || len(claude.MCPServers) != 0 {
		t.Fatalf("codex %+v\nclaude %+v", codex, claude)
	}
}

// TestHarnessToolRules_CodexMCPBlockReachesTheBrokerSet: a person's Block on a
// Codex CLI MCP tool compiles into Codex's own set, the set the broker reads
// for Codex's token, and into no other harness's.
func TestHarnessToolRules_CodexMCPBlockReachesTheBrokerSet(t *testing.T) {
	tools := harnessTools("claude-code", "codex-cli")
	tools[1].ToolRules = []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}
	_, sets := applyHarnessToolRules(types.RunPolicySpec{}, tools, types.ToolAllow)
	codex, ok := types.ToolEffectForHarness(sets, "codex-cli", "mcp__linear__delete_issue")
	claude, _ := types.ToolEffectForHarness(sets, "claude-code", "mcp__linear__delete_issue")
	if !ok || codex != types.ToolDeny || claude == types.ToolDeny {
		t.Fatalf("codex %q (%v), claude %q; sets %+v", codex, ok, claude, sets)
	}
	if harnessToolRulesRefusal(tools[1]) != nil {
		t.Fatal("a Codex MCP Block is refused")
	}
}

// TestMCPServerKeys_OrgClaimsItsKeyFirst: a person's own server named like an
// organisation's, placed first in the request, takes the suffix, so a policy
// Block on mcp__linear__delete_issue holds the organisation's delete_issue at
// the broker.
func TestMCPServerKeys_OrgClaimsItsKeyFirst(t *testing.T) {
	f := newComponentFixture(t)
	orgID := uuid.MustParse(compOrgID)
	orgDef := types.ComponentDefinition{Hosts: []string{"mcp.org-linear.test"},
		MCP: &types.ComponentMCP{URL: "https://mcp.org-linear.test/mcp", Transport: types.ComponentMCPTransportHTTP}}
	f.st.components = append(f.st.components, types.Component{ID: orgID, Name: "Linear", Version: 1, Definition: orgDef})
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	req := createRunRequest{Components: []types.ComponentRef{mcpInline("linear!"), {ID: &orgID}}}
	req.Tools = harnessTools("claude-code")
	spec := govDeployment()
	comps, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal.body)
	}
	byKey := map[string]string{}
	for _, m := range comps.dispatch().MCP {
		byKey[m.Key] = m.URL
	}
	if byKey["linear"] != "https://mcp.org-linear.test/mcp" || byKey["linear-2"] != "https://mcp.linear.test/mcp" {
		t.Fatalf("keys %v: the organisation's server must keep linear", byKey)
	}
}

// TestMCPServerKeys_OrgKeysFollowComponentIDNotAttachOrder: two organisation
// servers whose names cut to one key (a row saved before orgMCPKeyFree) take
// it in component-id order, so the key an organisation's rules name is the
// same server on every run whatever order it was attached in.
func TestMCPServerKeys_OrgKeysFollowComponentIDNotAttachOrder(t *testing.T) {
	low, high := uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("00000000-0000-0000-0000-000000000002")
	org := func(id uuid.UUID) attachedComponent {
		return attachedComponent{snapshot: types.RunComponent{Name: "Linear", ComponentID: &id,
			Definition: types.ComponentDefinition{MCP: &types.ComponentMCP{URL: "https://mcp.linear.test/mcp"}}}}
	}
	for name, attached := range map[string][]attachedComponent{
		"low first":  {org(low), org(high)},
		"high first": {org(high), org(low)},
	} {
		keys := mcpServerKeys(attached)
		for i, a := range attached {
			want := "linear"
			if *a.snapshot.ComponentID == high {
				want = "linear-2"
			}
			if keys[i] != want {
				t.Errorf("%s: component %s key = %q, want %q", name, a.snapshot.ComponentID, keys[i], want)
			}
		}
	}
}

// TestDispatchMCP_AutonomousRunNeverCarriesManagedMCP: Claude Code refuses
// --strict-mcp-config while managed-mcp.json exists, so no autonomous run is
// given it.
func TestDispatchMCP_AutonomousRunNeverCarriesManagedMCP(t *testing.T) {
	servers := []componentMCP{{Key: "linear", URL: "https://mcp.linear.test/mcp", Harnesses: []string{"claude-code", "codex-cli"}}}
	out := dispatchMCP(servers, &[]runner.InjectionGrant{}, "http://p:3128", false, map[string]string{})
	for _, f := range out.files {
		if f.Path == claudeMCPInteractivePath {
			t.Fatalf("autonomous run carries %s", f.Path)
		}
	}
	if len(out.files) != 1 {
		t.Fatalf("files %+v", out.files)
	}
}

// TestReviveMCPBindingsNarrows_BuiltinFirst: a built-in writes no
// run_components row, so dispatch numbers servers by their row: with
// [github built-in, Alpha, Beta] and Alpha erased, the revive keeps Beta.
func TestReviveMCPBindingsNarrows_BuiltinFirst(t *testing.T) {
	mcp := func(host string) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: []string{host}, MCP: &types.ComponentMCP{URL: "https://" + host + "/mcp", Transport: types.ComponentMCPTransportHTTP}}
	}
	comps := runComponents{attached: []attachedComponent{
		{builtin: &builtinGit{}},
		{snapshot: types.RunComponent{SelfDefined: true, Name: "Alpha", Definition: mcp("mcp.alpha.test"), MCPHarnesses: []string{"codex-cli"}}},
		{snapshot: types.RunComponent{SelfDefined: true, Name: "Beta", Definition: mcp("mcp.beta.test"), MCPHarnesses: []string{"codex-cli"}}},
	}}
	out := dispatchMCP(comps.dispatch().MCP, &[]runner.InjectionGrant{}, "http://p:3128", false, map[string]string{})
	cfg := proxy.Config{MCPServers: out.servers, MCPBindings: out.bindings}
	rows := []types.RunComponent{{Ordinal: 0, Erased: true, MCPHarnesses: []string{"codex-cli"}},
		{Ordinal: 1, Definition: mcp("mcp.beta.test"), MCPHarnesses: []string{"codex-cli"}}}
	narrowMCPBindings(&cfg, rows)
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Key != "beta" || !slices.Equal(cfg.MCPBindings[0].Servers, []string{"beta"}) {
		t.Fatalf("narrowed %+v %+v: want Beta kept and erased Alpha dropped", cfg.MCPServers, cfg.MCPBindings)
	}
}

// mcpHoldCase is one resolved policy met by one run: whether the Linear MCP
// server is attached, and whether the doors refuse it.
type mcpHoldCase struct {
	name    string
	rules   []types.ToolRule
	mcp     bool
	refused bool
}

// mcpHoldCases is the cases an inherited MCP hold is judged by, for a harness
// nothing can pause: an explicit hold on a tool of a bound server refuses, by
// name or by the hold default, and an exact allow lifts neither from the
// server's other tools; no rule at all (the broker's fallback), an allow or
// block default with no hold on the server, a hold on a server the run does not
// bind, and a run with no MCP server are admitted.
func mcpHoldCases(t *testing.T) []mcpHoldCase {
	t.Helper()
	example, err := os.ReadFile("../../examples/policies/autonomous-tool-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var autonomous types.RunPolicySpec
	if err := json.Unmarshal(example, &autonomous); err != nil {
		t.Fatal(err)
	}
	hold := func(tool string) types.ToolRule { return types.ToolRule{Tool: tool, Effect: types.ToolHold} }
	allow := func(tool string) types.ToolRule { return types.ToolRule{Tool: tool, Effect: types.ToolAllow} }
	return []mcpHoldCase{
		{"a hold on a tool of the bound server", []types.ToolRule{hold("mcp__linear__create_issue")}, true, true},
		{"a hold default with a bound server", autonomous.ToolRules, true, true},
		{"a hold default with no MCP server", autonomous.ToolRules, false, false},
		{"no rule at all with a bound server", nil, true, false},
		{"an exact allow does not lift a hold default from the server's other tools", []types.ToolRule{allow("mcp__linear__create_issue"), hold("*")}, true, true},
		{"an exact hold under an allow default", []types.ToolRule{hold("mcp__linear__create_issue"), allow("*")}, true, true},
		{"an allow default with a bound server", []types.ToolRule{allow("*")}, true, false},
		{"an exact allow over a block default", []types.ToolRule{allow("mcp__linear__create_issue"), {Tool: "*", Effect: types.ToolDeny}}, true, false},
		{"a hold on a server the run does not bind", []types.ToolRule{hold("mcp__github__create_issue")}, true, false},
		{"a hold on an MCP tool with no MCP server", []types.ToolRule{hold("mcp__linear__create_issue")}, false, false},
	}
}

// askMCPHold sends one run, of harness under workload, to one door under the
// deployment policy's tool rules and reports whether the door refused it for
// the hold, with sentence and no other.
func askMCPHold(t *testing.T, tc mcpHoldCase, door, harness string, workload map[string]any, sentence string) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.AgentImages = composeAgentImages
	f.srv.cfg.DefaultPolicy.ToolRules = tc.rules
	body := map[string]any{"experience": "background", "workload": workload, "no_repositories_or_drives": true,
		"tools": []map[string]any{{"id": harness, "kind": "harness"}}}
	if tc.mcp {
		body["components"] = []types.ComponentRef{mcpInline("Linear")}
	}
	w := f.ask(t, door, body)
	refused := w.Code == http.StatusUnprocessableEntity && errorReason(w) == reasonRequestFieldUnavailable
	if refused != tc.refused || (!refused && w.Code >= 300) {
		t.Fatalf("= %d %s, want refused=%v", w.Code, w.Body.String(), tc.refused)
	}
	if refused && decodeErrorBody(t, w).Error != sentence {
		t.Fatalf("refused with %s, want the sentence %q", w.Body.String(), sentence)
	}
}

// A background Codex CLI run is refused at every door when the policy explicitly
// holds a tool of an MCP server bound to it: Codex CLI cannot pause a call and
// the broker passes a held one.
func TestInheritedMCPHoldRefusesBackgroundCodex(t *testing.T) {
	for _, tc := range mcpHoldCases(t) {
		for _, door := range componentDoors {
			t.Run(tc.name+door, func(t *testing.T) {
				askMCPHold(t, tc, door, "codex-cli", map[string]any{"kind": "agent_task", "agent": "codex-cli", "task": "t"}, accessSentences["harness_no_hold"])
			})
		}
	}
}

// A command workload is refused at every door when the policy explicitly holds
// a tool of an MCP server bound to a harness it includes, Claude Code or Codex
// CLI: a harness a command starts is off the gated lane.
func TestInheritedMCPHoldRefusesACommandsHarness(t *testing.T) {
	for _, harness := range []string{"claude-code", "codex-cli"} {
		for _, tc := range mcpHoldCases(t) {
			for _, door := range componentDoors {
				t.Run(harness+"/"+tc.name+door, func(t *testing.T) {
					askMCPHold(t, tc, door, harness, map[string]any{"kind": "command", "command": "make"}, accessSentences["command_no_hold"])
				})
			}
		}
	}
}
