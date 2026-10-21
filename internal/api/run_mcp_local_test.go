// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// localInline is a person's own MCP server that runs in the sandbox, with a
// header secret for its one host and a config variable.
func localInline(name string) types.ComponentRef {
	return types.ComponentRef{Name: name, Inline: &types.ComponentDefinition{
		Hosts: []string{"api.files.test"},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
			Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}},
		Secrets: []types.ComponentSecret{{SecretName: compOwnSecret,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.files.test"}}},
		Config: map[string]string{"FILES_ROOT": "/work"},
	}}
}

// TestLocalMCP_OffRefusedAtSaveAndGate: with WARDYN_MCP_LOCAL_SERVERS off (the
// default) a server that runs in the sandbox is refused when saved and when a
// run attaches it, with a sentence naming the switch; on, both admit it.
func TestLocalMCP_OffRefusedAtSaveAndGate(t *testing.T) {
	c := types.Component{Name: "Files", Owner: "alice", Definition: *localInline("Files").Inline}
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	req := createRunRequest{Components: []types.ComponentRef{localInline("Files")}}
	req.Tools = harnessTools("claude-code")
	for _, on := range []string{"", "0", "1"} {
		t.Setenv(envMCPLocalServers, on)
		reason, msg := (&Server{}).componentSaveRefusal(context.Background(), c)
		spec := govDeployment()
		_, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
		if on == "1" {
			if reason != "" || refusal != nil {
				t.Fatalf("on: save %q %q, gate %+v", reason, msg, refusal)
			}
			continue
		}
		if reason != reasonComponentDefinitionInvalid || !strings.Contains(msg, "definition.mcp.transport: MCP servers that run in the sandbox are off on this deployment") ||
			!strings.HasSuffix(msg, envMCPLocalServers) {
			t.Errorf("%q save: %q %q", on, reason, msg)
		}
		if refusal == nil || refusal.body.Reason != reasonComponentDefinitionInvalid || !strings.Contains(refusal.body.Error, envMCPLocalServers) {
			t.Errorf("%q gate: %+v", on, refusal)
		}
	}
}

// TestRunComponents_LocalMCPServerIsRawEgress: a local server's hosts join the
// run's allowlist, its header secret is intercepted like any component's
// (never brokered), and dispatch carries its launch command and its own
// variable names, with no URL.
func TestRunComponents_LocalMCPServerIsRawEgress(t *testing.T) {
	t.Setenv(envMCPLocalServers, "1")
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	req := createRunRequest{Components: []types.ComponentRef{localInline("Files")}}
	req.Tools = harnessTools("claude-code", "codex-cli")
	spec := govDeployment()
	comps, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal.body)
	}
	if !slices.Contains(spec.AllowedDomains, "api.files.test") {
		t.Errorf("allowlist %v: a local server's hosts are raw egress", spec.AllowedDomains)
	}
	if len(comps.mcpHosts) != 0 || !slices.Equal(comps.mitmHosts, []string{"api.files.test"}) {
		t.Errorf("mitmHosts %v mcpHosts %v: a local server's header is intercepted, never brokered", comps.mitmHosts, comps.mcpHosts)
	}
	d := comps.dispatch()
	if len(d.MCP) != 1 || d.MCP[0].Key != "files" || d.MCP[0].URL != "" || d.MCP[0].Launch == nil || d.MCP[0].Credentialed ||
		!slices.Equal(d.MCP[0].EnvNames, []string{"FILES_ROOT"}) || !slices.Equal(d.MCP[0].Harnesses, []string{"claude-code", "codex-cli"}) {
		t.Errorf("dispatch MCP %+v: want the launch command and no URL", d.MCP)
	}
	if !slices.Contains(d.MITMHosts, "api.files.test:443") {
		t.Errorf("MITMHosts %v: a local server's header is intercepted at dispatch", d.MITMHosts)
	}
	if f := comps.attached[0].fact(0, comps, composer.Baseline{}); !f.TLSIntercept {
		t.Errorf("fact %+v: a local server's header is TLS-intercepted", f)
	}
}

// TestDispatchMCP_LocalServerInHarnessConfig: a local server takes no token
// and no broker entry; Claude Code's file carries its command beside the
// broker URLs, and Codex CLI gets its table, whose env_vars name the
// sandbox's variables and the server's own but never the broker URLs.
func TestDispatchMCP_LocalServerInHarnessConfig(t *testing.T) {
	launch := &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}
	servers := []componentMCP{
		{Ordinal: 0, Key: "files", Harnesses: []string{"claude-code", "codex-cli"}, Launch: launch, EnvNames: []string{"FILES_TOKEN", "FILES_ROOT"}},
		{Ordinal: 1, Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{"claude-code", "codex-cli"}},
	}
	var injections []runner.InjectionGrant
	env := map[string]string{"HTTPS_PROXY": "http://wardyn-proxy:3128", "PATH": "/usr/bin"}
	out := dispatchMCP(servers, &injections, "http://wardyn-proxy:3128", false, env)
	if len(out.servers) != 1 || out.servers[0].Key != "linear" || len(out.bindings) != 2 ||
		!slices.Equal(out.bindings[0].Servers, []string{"linear"}) || !slices.Equal(out.bindings[1].Servers, []string{"linear"}) {
		t.Fatalf("servers %+v bindings %+v: a local server is never at the broker", out.servers, out.bindings)
	}
	var claude struct {
		MCPServers map[string]struct {
			Type, URL, Command string
			Args               []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.files[0].Content, &claude); err != nil {
		t.Fatal(err)
	}
	if fs := claude.MCPServers["files"]; fs.Type != "stdio" || fs.Command != "npx" || !slices.Equal(fs.Args, launch.Args) || fs.URL != "" ||
		claude.MCPServers["linear"].Type != "http" {
		t.Errorf("claude file %s: want the local server's command beside the broker URL", out.files[0].Content)
	}
	want := "[mcp_servers.files]\ncommand = \"npx\"\nargs = [\"-y\",\"@example/mcp-files@1.4.2\"]\n" +
		"env_vars = [\"FILES_ROOT\",\"FILES_TOKEN\",\"HTTPS_PROXY\",\"PATH\"]\n"
	if env[codexMCPLocalEnv] != want || env[codexMCPEnv] == "" {
		t.Errorf("codex env %q, broker %q: want the local server's table", env[codexMCPLocalEnv], env[codexMCPEnv])
	}
	// Only a local server: no token is minted, and Codex gets no broker env.
	env = map[string]string{}
	if out := dispatchMCP(servers[:1], &injections, "http://wardyn-proxy:3128", true, env); len(out.bindings) != 0 || len(out.servers) != 0 ||
		env[codexMCPEnv] != "" || env[codexMCPLocalEnv] == "" || len(out.files) != 1 || out.files[0].Path != claudeMCPInteractivePath {
		t.Fatalf("local only: %+v env %v", out, env)
	}
}

// TestLocalMCPRulesRefusal: a hold or block on a local server's tools, by name
// or by default, refuses a run that turns the server on for a harness that
// cannot apply it (Codex CLI in any mode, a command's harness); Claude Code in
// an agent run applies it, an allow stands, and a remote server's rules are
// the broker's.
func TestLocalMCPRulesRefusal(t *testing.T) {
	local := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio}}
	remote := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.files.test/mcp"}}
	deny := func(tool string) []types.ToolRule { return []types.ToolRule{{Tool: tool, Effect: types.ToolDeny}} }
	cases := []struct {
		name     string
		def      types.ComponentDefinition
		harness  string
		taskMode string
		policy   []types.ToolRule
		own      client.IncludedTool
		refused  bool
	}{
		{"codex, policy block on a tool", local, "codex-cli", "", deny("mcp__files__write"), client.IncludedTool{}, true},
		{"codex, block default", local, "codex-cli", "", deny("*"), client.IncludedTool{}, true},
		{"codex, the person's own hold", local, "codex-cli", "", nil,
			client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "mcp__files__read", Effect: types.ToolHold}}}, true},
		{"codex, an allow", local, "codex-cli", "", []types.ToolRule{{Tool: "mcp__files__read", Effect: types.ToolAllow}}, client.IncludedTool{}, false},
		{"codex, another server's tool", local, "codex-cli", "", deny("mcp__linear__write"), client.IncludedTool{}, false},
		{"claude agent run applies it", local, "claude-code", "", deny("mcp__files__write"), client.IncludedTool{}, false},
		{"claude in a command", local, "claude-code", "exec", deny("mcp__files__write"), client.IncludedTool{}, true},
		{"codex, remote server", remote, "codex-cli", "", deny("mcp__files__write"), client.IncludedTool{}, false},
	}
	for _, tc := range cases {
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
		name := map[string]string{"codex-cli": "Codex CLI", "claude-code": "Claude Code"}[tc.harness]
		want := name + " can't apply rules for files's tools, so this run would be refused. Turn files off for " + name + ", or remove its rules for " + name + "."
		if got != nil && (got.body.Reason != reasonRequestFieldUnavailable || got.body.Error != want) {
			t.Errorf("%s: %+v", tc.name, got.body)
		}
	}
}

// TestLocalMCPRulesRefusal_AtTheDoors: a background Codex CLI run whose policy
// blocks a tool of its local server is refused at every door, where the
// remote server's broker would have applied it.
func TestLocalMCPRulesRefusal_AtTheDoors(t *testing.T) {
	t.Setenv(envMCPLocalServers, "1")
	for _, door := range componentDoors {
		f := newComponentFixture(t)
		f.srv.cfg.AgentImages = composeAgentImages
		f.srv.cfg.DefaultPolicy.ToolRules = []types.ToolRule{{Tool: "mcp__files__write", Effect: types.ToolDeny}}
		w := f.ask(t, door, map[string]any{"experience": "background", "no_repositories_or_drives": true,
			"workload":   map[string]any{"kind": "agent_task", "agent": "codex-cli", "task": "t"},
			"tools":      []map[string]any{{"id": "codex-cli", "kind": "harness"}},
			"components": []types.ComponentRef{localInline("Files")}})
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRequestFieldUnavailable ||
			decodeErrorBody(t, w).Error != "Codex CLI can't apply rules for files's tools, so this run would be refused. Turn files off for Codex CLI, or remove its rules for Codex CLI." {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body.String())
		}
	}
}

// TestSetupStatus_FeaturesMCPLocalServers: the authenticated setup status
// carries WARDYN_MCP_LOCAL_SERVERS as features.mcp_local_servers, to a member
// too, so the console offers local servers only where the API takes them.
func TestSetupStatus_FeaturesMCPLocalServers(t *testing.T) {
	srv := carrierServer(t, types.SiteConfig{}, nil, nil)
	for env, want := range map[string]bool{"": false, "0": false, "1": true} {
		t.Setenv(envMCPLocalServers, env)
		_, m := getJSON(t, srv, "/api/v1/setup/status", memberCookie(t))
		var features struct {
			MCPLocalServers *bool `json:"mcp_local_servers"`
		}
		if err := json.Unmarshal(m["features"], &features); err != nil || features.MCPLocalServers == nil || *features.MCPLocalServers != want {
			t.Errorf("%q: features %s, want mcp_local_servers %v", env, m["features"], want)
		}
	}
}

// TestAccessFacts_LocalMCPHasNoBrokerEnforcer: step 5 never says the broker
// enforces Codex CLI's rules when its only server runs in the sandbox, and the
// server's hosts are raw egress.
func TestAccessFacts_LocalMCPHasNoBrokerEnforcer(t *testing.T) {
	def := *localInline("Files").Inline
	comps := runComponents{attached: []attachedComponent{{source: componentSourceInline,
		snapshot: types.RunComponent{SelfDefined: true, Name: "Files", Definition: def, MCPHarnesses: []string{"codex-cli"}}}}}
	req := createRunRequest{}
	req.Tools = harnessTools("codex-cli")
	b := &accessBuilder{in: accessInputs{req: &req, comps: comps}, hosts: map[string]*client.AccessHost{}}
	b.componentRow(componentFact{ID: "inline:0", Name: "Files", Status: componentReady, Hosts: def.Hosts})
	if row := b.out.Rows[0]; row.MCP == nil || row.MCP.Transport != types.ComponentMCPTransportStdio || b.hosts["api.files.test"].Via != viaEgress {
		t.Fatalf("row %+v host %+v", row, b.hosts["api.files.test"])
	}
	if codex := b.toolPolicyFact("codex-cli"); codex.Enforcer == toolEnforcerMCPBroker || !slices.Equal(codex.MCPServers, []string{"files"}) {
		t.Fatalf("codex %+v", codex)
	}
}

// mcpTokenRE is a harness's broker token inside a broker URL, and mcpDigestRE
// its digest, both random per dispatch.
var (
	mcpTokenRE  = regexp.MustCompile(`/wardyn/mcp/[A-Za-z0-9_-]{43}/`)
	mcpDigestRE = regexp.MustCompile(`"sha256": "[0-9a-f]{64}"`)
)

// TestDispatchMCP_RemoteOnlyGolden: with no server that runs in the sandbox (the
// only kind a deployment with WARDYN_MCP_LOCAL_SERVERS off can attach), what
// dispatch writes for remote servers is byte-for-byte what it wrote before
// local servers existed: the Claude Code file, the Codex CLI env and the
// broker's config. testdata/mcp_dispatch_remote.golden was captured from the
// code before the change.
func TestDispatchMCP_RemoteOnlyGolden(t *testing.T) {
	grant := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	var got []byte
	for _, interactive := range []bool{false, true} {
		injections := []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: "mcp.linear.test"}}}
		servers := []componentMCP{
			{Ordinal: 0, Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{"claude-code", "codex-cli"}, Credentialed: true},
			{Ordinal: 2, Key: "public", URL: "https://mcp.public.test/mcp", Host: "mcp.public.test", Harnesses: []string{"claude-code"}},
		}
		env := map[string]string{"HTTPS_PROXY": "http://wardyn-proxy:3128"}
		out := dispatchMCP(servers, &injections, "http://wardyn-proxy:3128/", interactive, env)
		files := map[string]string{}
		for _, f := range out.files {
			files[f.Path] = string(f.Content)
		}
		b, err := json.MarshalIndent(map[string]any{"interactive": interactive, "files": files, "env": env,
			"servers": out.servers, "bindings": out.bindings, "injections": injections}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(append(got, b...), '\n')
	}
	got = mcpDigestRE.ReplaceAll(mcpTokenRE.ReplaceAll(got, []byte("/wardyn/mcp/<token>/")), []byte(`"sha256": "<digest>"`))
	want, err := os.ReadFile("testdata/mcp_dispatch_remote.golden")
	if err != nil || string(got) != string(want) {
		t.Fatalf("dispatch output drifted from the golden (%v):\n%s", err, got)
	}
}

// TestRevive_LocalMCPMeetsTheSwitchAsTheQueueDoes: a rebooted revive starts the
// agent again, which admits the run anew, so a local server in the run's frozen
// components meets WARDYN_MCP_LOCAL_SERVERS as a queued run's admission does.
// Turned off after launch, the revive is refused with the gate's reason and
// sentence and the agent is not started; still on, the run revives. A revive
// that leaves the agent running keeps its server.
func TestRevive_LocalMCPMeetsTheSwitchAsTheQueueDoes(t *testing.T) {
	def := *localInline("Files").Inline
	def.Secrets = nil
	row := types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "Files", Definition: def}
	revive := func(f *reviveFixture) (int, errorBody) {
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	t.Run("on", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "1")
		f, sr := newRebootFixture(t)
		f.withRunComponents(row)
		if code, body := revive(f); code != http.StatusOK || !slices.Equal(sr.starts(), []int{1}) {
			t.Fatalf("revive = %d %+v, starts %v; want 200 and the agent started", code, body, sr.starts())
		}
	})

	t.Run("off after launch", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		gate := newComponentFixture(t)
		r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
		req := createRunRequest{Components: []types.ComponentRef{localInline("Files")}}
		req.Tools = harnessTools("claude-code")
		spec := govDeployment()
		_, queued := gate.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
		if queued == nil {
			t.Fatal("the gate admitted a local server with the switch off")
		}
		f, sr := newRebootFixture(t)
		f.withRunComponents(row)
		code, body := revive(f)
		if code != queued.status || body.Reason != queued.body.Reason || body.Error != queued.body.Error {
			t.Fatalf("revive = %d %q %q; want the gate's %d %q %q", code, body.Reason, body.Error,
				queued.status, queued.body.Reason, queued.body.Error)
		}
		if got := sr.starts(); len(got) != 0 {
			t.Errorf("StartSandbox calls = %v; a refused revive started the agent", got)
		}
		f.assertReviveRefused(t, reasonComponentDefinitionInvalid)
	})

	t.Run("off, agent still running", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f := newReviveFixture(t)
		f.withRunComponents(row)
		if code, body := revive(f); code != http.StatusOK {
			t.Fatalf("revive = %d %+v; want 200, the running agent keeps its server", code, body)
		}
	})
}

// TestRevive_ErasedMCPRowMeetsTheSwitch: an erased row keeps only the harnesses
// its MCP server was turned on for, so with WARDYN_MCP_LOCAL_SERVERS off a
// rebooted revive refuses it in the org form, without a field of the gone
// definition, and the agent is not started. The tombstone of a remote server is
// the same row, so it is refused too. An erased row with no MCP server, a live
// local row, the switch on and a revive that leaves the agent running are
// unchanged.
func TestRevive_ErasedMCPRowMeetsTheSwitch(t *testing.T) {
	def := *localInline("Files").Inline
	def.Secrets = nil
	erasedMCP := types.RunComponent{SelfDefined: true, Erased: true, MCPHarnesses: []string{"claude-code"}}
	revive := func(f *reviveFixture) (int, errorBody) {
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	t.Run("erased local row, off", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f, sr := newRebootFixture(t)
		f.withRunComponents(erasedMCP)
		code, body := revive(f)
		const want = "components[0]: this component has a definition that is not valid here. Ask your admin."
		if code != http.StatusUnprocessableEntity || body.Reason != reasonComponentDefinitionInvalid || body.Error != want {
			t.Fatalf("revive = %d %q %q; want 422 %q %q", code, body.Reason, body.Error, reasonComponentDefinitionInvalid, want)
		}
		if got := sr.starts(); len(got) != 0 {
			t.Errorf("StartSandbox calls = %v; a refused revive started the agent", got)
		}
		f.assertReviveRefused(t, reasonComponentDefinitionInvalid)
	})

	t.Run("erased non-MCP row, off", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f, sr := newRebootFixture(t)
		f.withRunComponents(reviveErasedRow)
		if code, body := revive(f); code != http.StatusOK || !slices.Equal(sr.starts(), []int{1}) {
			t.Fatalf("revive = %d %+v, starts %v; want 200 and the agent started", code, body, sr.starts())
		}
	})

	t.Run("live local row, off", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f, sr := newRebootFixture(t)
		f.withRunComponents(types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "Files", Definition: def, MCPHarnesses: []string{"claude-code"}})
		code, body := revive(f)
		if code != http.StatusUnprocessableEntity || !strings.HasPrefix(body.Error, "components[0]: inline.mcp.transport: ") || len(sr.starts()) != 0 {
			t.Fatalf("revive = %d %+v, starts %v; want 422 naming inline.mcp.transport, no start", code, body, sr.starts())
		}
		f.assertReviveRefused(t, reasonComponentDefinitionInvalid)
	})

	t.Run("erased remote row, off", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f, sr := newRebootFixture(t)
		// Erasure clears the transport, so a remote server's row reads as erasedMCP does.
		f.withRunComponents(types.RunComponent{SelfDefined: true, Erased: true, MCPHarnesses: []string{"codex-cli"}})
		if code, body := revive(f); code != http.StatusUnprocessableEntity || len(sr.starts()) != 0 {
			t.Fatalf("revive = %d %+v, starts %v; want 422, no start", code, body, sr.starts())
		}
		f.assertReviveRefused(t, reasonComponentDefinitionInvalid)
	})

	t.Run("erased local row, on", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "1")
		f, sr := newRebootFixture(t)
		f.withRunComponents(erasedMCP)
		if code, body := revive(f); code != http.StatusOK || !slices.Equal(sr.starts(), []int{1}) {
			t.Fatalf("revive = %d %+v, starts %v; want 200 and the agent started", code, body, sr.starts())
		}
	})

	t.Run("erased local row, off, agent still running", func(t *testing.T) {
		t.Setenv(envMCPLocalServers, "")
		f := newReviveFixture(t)
		f.withRunComponents(erasedMCP)
		if code, body := revive(f); code != http.StatusOK {
			t.Fatalf("revive = %d %+v; want 200, the running agent keeps its server", code, body)
		}
	})
}
