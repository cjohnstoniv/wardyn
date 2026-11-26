// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localInline is a person's own MCP server that starts from a command, with one host.
func localInline(name string) types.ComponentRef {
	return types.ComponentRef{Name: name, Inline: &types.ComponentDefinition{
		Hosts: []string{"api.files.test"},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
			Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}},
	}}
}

// mcpSiteStore answers the site config, or fails to.
type mcpSiteStore struct {
	store.Store
	site types.SiteConfig
	err  error
}

func (m mcpSiteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return m.site, m.err }

// mcpKindsRunner advertises the deployment executor's MCP sandbox kinds.
type mcpKindsRunner struct {
	runner.Runner
	kinds []types.MachineKind
}

func (r mcpKindsRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "fake", MCPSandboxKinds: r.kinds}, nil
}

// localMCPSite is a site config whose Local MCP servers setting is v ("" leaves the key out).
func localMCPSite(v string) types.SiteConfig {
	if v == "" {
		return types.SiteConfig{}
	}
	s := types.LocalMCPSetting(v)
	return types.SiteConfig{Components: &types.ComponentSettings{LocalMCPServers: &s}}
}

// stdioComps is n attached servers that start from a command, keyed files, files-2, ...
func stdioComps(n int) runComponents {
	var comps runComponents
	for range n {
		comps.attached = append(comps.attached, attachedComponent{snapshot: types.RunComponent{Name: "Files", Definition: *localInline("Files").Inline}})
	}
	return comps
}

// mcpGatePlacement is one placement row of the matrix.
type mcpGatePlacement struct {
	name  string
	req   types.Placement
	place runPlacement
	kinds []types.MachineKind // what the deployment's executor advertises, intersected across its executors
	local bool
}

var mcpGatePlacements = []mcpGatePlacement{
	{name: "remote", req: types.PlacementRemote, kinds: []types.MachineKind{types.MachineContainer}},
	{name: "local not pooled", req: types.PlacementLocal, local: true},
	{name: "local in a user pool", place: runPlacement{pooled: true, placement: types.PlacementLocal}, local: true},
	{name: "a user pool's remote-hosted member", place: runPlacement{pooled: true, placement: types.PlacementRemote},
		kinds: []types.MachineKind{types.MachineContainer}},
	{name: "an organisation pool whose executors share no kind", place: runPlacement{pooled: true, placement: types.PlacementRemote}},
}

// TestMCPSandboxGate is the gate's matrix: the switch, the organisation's setting (unset, off, on, or a site
// config that cannot be read), the machine kind, the placement and the count, with each door's outcome. The
// save door and the admission door judge the switch alone; the fold judges the rest, in order.
func TestMCPSandboxGate(t *testing.T) {
	for _, flag := range []string{"", "1"} {
		for _, setting := range []string{"unset", "off", "on", "read error"} {
			for _, kind := range []types.MachineKind{types.MachineContainer, types.MachineLinuxVM, types.MachineMacOSVM} {
				for _, pl := range mcpGatePlacements {
					for _, count := range []int{4, 5} {
						name := strings.Join([]string{"flag=" + flag, setting, string(kind), pl.name, map[int]string{4: "4", 5: "5"}[count]}, "/")
						t.Run(name, func(t *testing.T) {
							t.Setenv(envMCPOwnSandbox, flag)
							mcpGateMatrixRow(t, flag, setting, kind, pl, count)
						})
					}
				}
			}
		}
	}
}

func mcpGateMatrixRow(t *testing.T, flag, setting string, kind types.MachineKind, pl mcpGatePlacement, count int) {
	t.Helper()
	st := mcpSiteStore{site: localMCPSite(map[string]string{"off": "off", "on": "on"}[setting])}
	if setting == "read error" {
		st.err = errors.New("site config unreadable")
	}
	s := &Server{}
	s.cfg.Store, s.cfg.Runner = st, mcpKindsRunner{kinds: pl.kinds}
	off := flag == "" || setting == "off" || setting == "read error"

	// The save door: a read error refuses as off.
	if got, want := s.mcpSaveProblem(context.Background(), *localInline("Files").Inline), map[bool]string{true: mcpSaveOffSentence}[off]; got != want {
		t.Fatalf("save = %q, want %q", got, want)
	}
	// The admission door: a read error is answered before step 2, with a 500 (TestMCPSandboxGateAdmissionReadError).
	if setting == "read error" {
		return
	}
	c := stdioComps(1).attached[0]
	if refused := mcpAdmissionRefusal(st.site, c, 0); off != (refused != nil) {
		t.Fatalf("admission = %+v, want refused %v", refused, off)
	} else if off {
		want := "MCP servers that start from a command are turned off here. Remove files, or ask an administrator."
		if refused.refusal.status != http.StatusUnprocessableEntity || refused.reason != reasonMCPOwnSandboxOff || refused.refusal.body.Error != want {
			t.Fatalf("admission = %d %q %q, want 422 %q %q", refused.refusal.status, refused.reason, refused.refusal.body.Error, reasonMCPOwnSandboxOff, want)
		}
		return
	}
	// The fold: macOS VM, Linux VM, local or a kind the executor lacks, then the count.
	req := createRunRequest{MachineKind: string(kind)}
	req.Placement = pl.req
	got := s.mcpSandboxRefusal(context.Background(), req, pl.place, stdioComps(count))
	wantReason, want := "", ""
	switch {
	case kind == types.MachineMacOSVM:
		wantReason, want = reasonMCPOwnSandboxUnsupported, "A macOS VM run can't start an MCP server in its own sandbox. Choose another machine, or remove files."
	case kind == types.MachineLinuxVM:
		wantReason, want = reasonMCPOwnSandboxUnsupported, "Local MCP servers can't run beside a Linux VM yet. Choose a container machine, use a remote (HTTPS) MCP server, or remove files."
	case pl.local || !slices.Contains(pl.kinds, kind):
		wantReason, want = reasonMCPOwnSandboxUnsupported, "This runner can't start an MCP server in its own sandbox. Choose a runner that can, or remove files."
	case count > mcpSandboxMax:
		wantReason, want = reasonMCPSandboxLimit, "A run can have at most 4 MCP servers in their own sandboxes. Remove one, then launch again."
	}
	if want == "" {
		if got != nil {
			t.Fatalf("fold refused %+v, want admitted", got.body)
		}
		return
	}
	if got == nil || got.status != http.StatusUnprocessableEntity || got.body.Reason != wantReason || got.body.Error != want || got.decision != nil {
		t.Fatalf("fold = %+v, want 422 %q %q and no authz decision", got, wantReason, want)
	}
}

// TestMCPSandboxGateAdmissionReadError: the admission door answers a site config it cannot read with a 500,
// before the switch is judged.
func TestMCPSandboxGateAdmissionReadError(t *testing.T) {
	t.Setenv(envMCPOwnSandbox, "1")
	f := newComponentFixture(t)
	f.srv.cfg.Store = mcpSiteErrComponentStore{f.st}
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	req := createRunRequest{Components: []types.ComponentRef{localInline("Files")}}
	req.Tools = harnessTools("claude-code")
	spec := govDeployment()
	_, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal == nil || refusal.err == nil {
		t.Fatalf("refusal %+v, want a server error", refusal)
	}
}

// mcpSiteErrComponentStore is the component fixture's store with a site config that cannot be read.
type mcpSiteErrComponentStore struct{ *componentTestStore }

func (mcpSiteErrComponentStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config unreadable")
}

// TestLinuxVMRunGetsD3VM: with the switch and the setting on, a Linux VM run with such a server answers
// the Linux VM sentence whatever its placement, before the executor's kinds and the count are read.
func TestLinuxVMRunGetsD3VM(t *testing.T) {
	t.Setenv(envMCPOwnSandbox, "1")
	const want = "Local MCP servers can't run beside a Linux VM yet. Choose a container machine, use a remote (HTTPS) MCP server, or remove files."
	for _, pl := range mcpGatePlacements {
		for _, kinds := range [][]types.MachineKind{nil, {types.MachineContainer, types.MachineLinuxVM}} {
			s := &Server{}
			s.cfg.Store, s.cfg.Runner = mcpSiteStore{site: localMCPSite("on")}, mcpKindsRunner{kinds: kinds}
			req := createRunRequest{MachineKind: string(types.MachineLinuxVM)}
			req.Placement = pl.req
			got := s.mcpSandboxRefusal(context.Background(), req, pl.place, stdioComps(5))
			if got == nil || got.body.Reason != reasonMCPOwnSandboxUnsupported || got.body.Error != want {
				t.Errorf("%s, kinds %v: %+v, want the Linux VM sentence", pl.name, kinds, got)
			}
		}
	}
}

// TestMCPSandboxGatePassesRemoteServers: a run whose MCP servers are all remote meets no step of the gate,
// whatever the switch, the machine or the placement.
func TestMCPSandboxGatePassesRemoteServers(t *testing.T) {
	remote := attachedComponent{snapshot: types.RunComponent{Name: "Linear", Definition: types.ComponentDefinition{
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.linear.test/mcp"}}}}
	s := &Server{}
	s.cfg.Store, s.cfg.Runner = mcpSiteStore{err: errors.New("unreadable")}, mcpKindsRunner{}
	for _, kind := range []types.MachineKind{types.MachineContainer, types.MachineLinuxVM, types.MachineMacOSVM} {
		req := createRunRequest{MachineKind: string(kind)}
		req.Placement = types.PlacementLocal
		if got := s.mcpSandboxRefusal(context.Background(), req, runPlacement{}, runComponents{attached: []attachedComponent{remote}}); got != nil {
			t.Errorf("%s: %+v", kind, got.body)
		}
	}
	if got := s.mcpSaveProblem(context.Background(), remote.snapshot.Definition); got != "" {
		t.Errorf("save: %q", got)
	}
	if got := mcpAdmissionRefusal(types.SiteConfig{}, remote, 0); got != nil {
		t.Errorf("admission: %+v", got)
	}
}

// TestMCPOwnSandboxSwitchAtSaveAndAdmission: through the real save and run doors, the switch off or the
// organisation's setting off refuses such a server when saved and when a run attaches it; both on, an unset
// setting included, admit it.
func TestMCPOwnSandboxSwitchAtSaveAndAdmission(t *testing.T) {
	c := types.Component{Name: "Files", Owner: "alice", Definition: *localInline("Files").Inline}
	for _, tc := range []struct {
		flag, setting string
		off           bool
	}{{"", "", true}, {"0", "on", true}, {"1", "off", true}, {"1", "", false}, {"1", "on", false}} {
		t.Setenv(envMCPOwnSandbox, tc.flag)
		f := newComponentFixture(t)
		f.st.siteConfig = localMCPSite(tc.setting)
		reason, msg := f.srv.componentSaveRefusal(context.Background(), c)
		r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
		req := createRunRequest{Components: []types.ComponentRef{localInline("Files")}}
		req.Tools = harnessTools("claude-code")
		spec := govDeployment()
		_, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
		if !tc.off {
			if reason != "" || refusal != nil {
				t.Errorf("%+v: save %q %q, gate %+v", tc, reason, msg, refusal)
			}
			if slices.Contains(spec.AllowedDomains, "api.files.test") {
				t.Errorf("%+v: allowlist %v: such a server's hosts are not the agent's", tc, spec.AllowedDomains)
			}
			continue
		}
		if reason != reasonComponentDefinitionInvalid || msg != "invalid component: definition.mcp.transport: MCP servers that start from a command are turned off here." {
			t.Errorf("%+v save: %q %q", tc, reason, msg)
		}
		if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonMCPOwnSandboxOff ||
			refusal.body.Error != "MCP servers that start from a command are turned off here. Remove files, or ask an administrator." {
			t.Errorf("%+v gate: %+v", tc, refusal)
		}
	}
}

// TestRawEgressHostsNoneForStdioServer: a server that starts from a command adds none of its hosts to the
// agent's allowlist; a remote server's hosts, less its broker host, are unchanged.
func TestRawEgressHostsNoneForStdioServer(t *testing.T) {
	if got := rawEgressHosts(*localInline("Files").Inline); len(got) != 0 {
		t.Errorf("stdio: %v, want none", got)
	}
	remote := types.ComponentDefinition{Hosts: []string{"mcp.linear.test", "api.linear.test"},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.linear.test/mcp"}}
	if got := rawEgressHosts(remote); !slices.Equal(got, []string{"api.linear.test"}) {
		t.Errorf("remote: %v", got)
	}
}

// TestDispatchMCP_StdioServerSkipped: dispatch never writes a server that starts from a command into the
// agent's configuration: no broker entry, no token, nothing in Claude Code's file or Codex CLI's env.
func TestDispatchMCP_StdioServerSkipped(t *testing.T) {
	launch := &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}
	servers := []componentMCP{
		{Ordinal: 0, Key: "files", Harnesses: []string{"claude-code", "codex-cli"}, Launch: launch, EnvNames: []string{"FILES_ROOT"}},
		{Ordinal: 1, Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{"claude-code", "codex-cli"}},
	}
	var injections []runner.InjectionGrant
	env := map[string]string{"HTTPS_PROXY": "http://wardyn-proxy:3128"}
	out := dispatchMCP(servers, withholding{}, &injections, "http://wardyn-proxy:3128", false, env)
	if len(out.servers) != 1 || out.servers[0].Key != "linear" || len(out.bindings) != 2 ||
		!slices.Equal(out.bindings[0].Servers, []string{"linear"}) || len(out.files) != 1 ||
		strings.Contains(string(out.files[0].Content), "files") || strings.Contains(env[codexMCPEnv], "files") || len(env) != 2 {
		t.Fatalf("out %+v env %v: the stdio server reached the agent's configuration", out, env)
	}
	env = map[string]string{}
	if out := dispatchMCP(servers[:1], withholding{}, &injections, "http://wardyn-proxy:3128", true, env); len(out.bindings) != 0 ||
		len(out.servers) != 0 || len(out.files) != 0 || len(env) != 0 {
		t.Fatalf("stdio only: %+v env %v", out, env)
	}
}

// mcpTokenRE is a harness's broker token inside a broker URL, and mcpDigestRE
// its digest, both random per dispatch.
var (
	mcpTokenRE  = regexp.MustCompile(`/wardyn/mcp/[A-Za-z0-9_-]{43}/`)
	mcpDigestRE = regexp.MustCompile(`"sha256": "[0-9a-f]{64}"`)
)

// TestDispatchMCP_RemoteOnlyGolden: what dispatch writes for remote servers is
// byte-for-byte what it wrote before servers that start from a command
// existed: the Claude Code file, the Codex CLI env and the broker's config.
// testdata/mcp_dispatch_remote.golden was captured from the code before them.
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
		out := dispatchMCP(servers, withholding{}, &injections, "http://wardyn-proxy:3128/", interactive, env)
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

// TestSetupFeatures_MCPOwnSandbox: mcp_own_sandbox is the switch; local_mcp_servers is the switch, a site
// config that could be read and the organisation's setting together; mcp_local_servers always reads false.
func TestSetupFeatures_MCPOwnSandbox(t *testing.T) {
	for _, tc := range []struct {
		flag             string
		localMCP         bool
		ownSandbox, both bool
	}{{"", true, false, false}, {"1", false, true, false}, {"1", true, true, true}} {
		t.Setenv(envMCPOwnSandbox, tc.flag)
		f := setupFeatures(false, false, tc.localMCP)
		if f.MCPOwnSandbox != tc.ownSandbox || f.LocalMCPServers != tc.both || f.MCPLocalServers {
			t.Errorf("%+v: %+v", tc, f)
		}
	}
	t.Setenv(envMCPOwnSandbox, "1")
	t.Setenv("WARDYN_MCP_LOCAL_SERVERS", "1")
	for setting, want := range map[string]bool{"": true, "on": true, "off": false} {
		srv := carrierServer(t, localMCPSite(setting), nil, nil)
		_, m := getJSON(t, srv, "/api/v1/setup/status", memberCookie(t))
		var features struct {
			Own   bool  `json:"mcp_own_sandbox"`
			Local bool  `json:"local_mcp_servers"`
			Old   *bool `json:"mcp_local_servers"`
		}
		if err := json.Unmarshal(m["features"], &features); err != nil || !features.Own || features.Local != want || features.Old == nil || *features.Old {
			t.Errorf("setting %q: features %s", setting, m["features"])
		}
	}
}

// siteErrCapStore is carrierServer's store with a site config that cannot be read.
type siteErrCapStore struct{ *capStore }

func (siteErrCapStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config unreadable")
}

// TestSetupStatusLocalMCPServers: features.local_mcp_servers is the switch and the organisation's setting
// together, through GET /setup/status: the switch off with the setting on reads false, both on read true, and a
// site config that cannot be read reads false while mcp_own_sandbox still shows the switch.
func TestSetupStatusLocalMCPServers(t *testing.T) {
	type features struct {
		Own   bool `json:"mcp_own_sandbox"`
		Local bool `json:"local_mcp_servers"`
	}
	read := func(srv *Server) features {
		_, m := getJSON(t, srv, "/api/v1/setup/status", memberCookie(t))
		var f features
		if err := json.Unmarshal(m["features"], &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, tc := range []struct {
		name, flag string
		readErr    bool
		want       features
	}{
		{"own sandbox off, setting on", "", false, features{}},
		{"both on", "1", false, features{Own: true, Local: true}},
		{"site config unreadable", "1", true, features{Own: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMCPOwnSandbox, tc.flag)
			srv := carrierServer(t, localMCPSite("on"), nil, nil)
			if tc.readErr {
				h := newHarness(t)
				cfg := baseTestConfig(h, siteErrCapStore{&capStore{Store: r3IntegStore{}, site: localMCPSite("on")}})
				cfg.OIDC = &oidc.Authenticator{}
				cfg.ADOEntra = adoTestEntraSource
				cfg.Secrets = &memSecrets{m: map[string][]byte{}}
				cfg.MaskRegistry = secretmask.NewRegistry()
				srv = New(cfg)
			}
			if got := read(srv); got != tc.want {
				t.Errorf("features = %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Setenv(envMCPOwnSandbox, "1")
	if setupFeatures(false, false, localMCPOnRead(localMCPSite("on"), false)).LocalMCPServers {
		t.Error("a site config that could not be read turned local_mcp_servers on")
	}
}

// TestReviveMCPSandboxRefusal: a rebooted revive of a run with a server that starts from a command is refused
// with the switch-off sentence while the switch or the setting is off (a site config that cannot be read reads
// as off), and with the earlier-release sentence otherwise, since every such run held its server inside the agent's sandbox. The agent is not
// started. An erased MCP row is refused the same way; an erased row with no MCP server, and a revive that
// leaves the agent running, are unchanged.
func TestReviveMCPSandboxRefusal(t *testing.T) {
	def := *localInline("Files").Inline
	row := types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "Files", Definition: def, MCPHarnesses: []string{"claude-code"}}
	erasedMCP := types.RunComponent{SelfDefined: true, Erased: true, MCPHarnesses: []string{"claude-code"}}
	const (
		d6       = "files was not restarted: MCP servers that start from a command are turned off here. End the run, or ask an administrator."
		d6Legacy = "files was not restarted: it ran inside the agent's sandbox, which this version no longer allows. End the run, then start a new one."
	)
	revive := func(f *reviveFixture) (int, errorBody) {
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}
	for _, tc := range []struct {
		name, flag, setting string
		comp                types.RunComponent
		want                string
	}{
		{"switch off", "", "", row, d6},
		{"setting off", "1", "off", row, d6},
		{"both on", "1", "", row, d6Legacy},
		{"erased MCP row, off", "", "", erasedMCP, strings.Replace(d6, "files", "mcp", 1)},
		{"erased MCP row, on", "1", "on", erasedMCP, strings.Replace(d6Legacy, "files", "mcp", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMCPOwnSandbox, tc.flag)
			f, sr := newRebootFixture(t)
			f.rs.site = localMCPSite(tc.setting)
			f.withRunComponents(tc.comp)
			code, body := revive(f)
			if code != http.StatusUnprocessableEntity || body.Reason != reasonComponentDefinitionInvalid || body.Error != tc.want {
				t.Fatalf("revive = %d %q %q; want 422 %q %q", code, body.Reason, body.Error, reasonComponentDefinitionInvalid, tc.want)
			}
			if got := sr.starts(); len(got) != 0 {
				t.Errorf("StartSandbox calls = %v; a refused revive started the agent", got)
			}
			f.assertReviveRefused(t, reasonComponentDefinitionInvalid)
		})
	}
	t.Run("site config unreadable reads as off", func(t *testing.T) {
		t.Setenv(envMCPOwnSandbox, "1")
		f, _ := newRebootFixture(t)
		cs := f.withRunComponents(row)
		cs.siteErr = errors.New("site config unreadable")
		got := f.srv.reviveMCPSandboxRefusal(context.Background(), f.run, true, types.ActorHuman, "admin")
		if got == nil || got.reason != reasonComponentDefinitionInvalid || got.msg != d6 {
			t.Fatalf("refusal %+v, want the switch-off sentence", got)
		}
	})
	t.Run("erased non-MCP row", func(t *testing.T) {
		t.Setenv(envMCPOwnSandbox, "")
		f, sr := newRebootFixture(t)
		f.withRunComponents(reviveErasedRow)
		if code, body := revive(f); code != http.StatusOK || !slices.Equal(sr.starts(), []int{1}) {
			t.Fatalf("revive = %d %+v, starts %v; want 200 and the agent started", code, body, sr.starts())
		}
	})
	t.Run("agent still running", func(t *testing.T) {
		t.Setenv(envMCPOwnSandbox, "")
		f := newReviveFixture(t)
		f.withRunComponents(row)
		if code, body := revive(f); code != http.StatusOK {
			t.Fatalf("revive = %d %+v; want 200, the running agent keeps its server until the run ends", code, body)
		}
	})
}

// TestEnvTogglesMCPOwnSandbox: the new switch is read at boot with the others, and the removed one is not.
func TestEnvTogglesMCPOwnSandbox(t *testing.T) {
	if !slices.Contains(envToggles, envMCPOwnSandbox) || slices.Contains(envToggles, "WARDYN_MCP_LOCAL_SERVERS") {
		t.Errorf("envToggles = %v", envToggles)
	}
}

// TestMCPSandboxSentencesMatchCanon finds each sentence const, byte for byte, among the approved strings: the
// templates of the refusal sentence table (the narrower overlay's with its drawn field path) and the table of
// strings with no frame. It reads the approved strings file named by MCP_SANDBOX_STRINGS and is skipped where
// it is not set.
func TestMCPSandboxSentencesMatchCanon(t *testing.T) {
	path := os.Getenv("MCP_SANDBOX_STRINGS")
	if path == "" {
		t.Skip("MCP_SANDBOX_STRINGS names no approved strings file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canon := map[string]bool{}
	for _, cell := range append(mcpCanonColumn(t, string(raw), "## D sentences", 2), mcpCanonColumn(t, string(raw), "## Strings with no frame", 2)...) {
		canon[strings.Trim(cell, "`")] = true
	}
	for _, got := range []string{
		mcpOwnSandboxOffSentence, mcpOwnSandboxUnsupportedSentence, mcpOwnSandboxMacSentence, mcpOwnSandboxLinuxVMSentence,
		mcpSandboxLimitSentence, mcpReviveOffSentence, mcpReviveLegacySentence, mcpSaveOffSentence, mcpRunnerUnnamed,
		"mcp.sandbox.policy.overlay.allowed_domains: " + mcpNarrowerWidensSentence,
	} {
		if !canon[got] {
			t.Errorf("const %q is not an approved string", got)
		}
	}
}

// mcpCanonColumn is cell col of each row of the table under heading.
func mcpCanonColumn(t *testing.T, doc, heading string, col int) []string {
	t.Helper()
	_, after, ok := strings.Cut(doc, "\n"+heading)
	if !ok {
		t.Fatalf("no %q section", heading)
	}
	if next := strings.Index(after, "\n## "); next >= 0 {
		after = after[:next]
	}
	var out []string
	for _, line := range strings.Split(after, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		if cells := strings.Split(strings.Trim(line, "|"), "|"); len(cells) > col {
			out = append(out, strings.TrimSpace(cells[col]))
		}
	}
	if len(out) < 3 {
		t.Fatalf("%q: read %d rows; the table is not where the test looks", heading, len(out))
	}
	return out
}

// TestMCPSandboxGateNarrowerOverlay is the matrix's step 6: the fold, at preview, preflight and create alike, admits a
// narrower overlay within "same" computed from the folded policy, and refuses one that names a host outside
// it with the overlay's field path.
func TestMCPSandboxGateNarrowerOverlay(t *testing.T) {
	i := slices.IndexFunc(runFoldSteps, func(st runFoldStep) bool {
		return strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name(), ".stepMCPSandbox")
	})
	if i < 0 || runFoldSteps[i].modes != foldAll {
		t.Fatalf("stepMCPSandbox is not a fold step at every door")
	}
	s := &Server{}
	s.cfg.Store, s.cfg.Runner = mcpSiteStore{site: localMCPSite("on")}, mcpKindsRunner{kinds: []types.MachineKind{types.MachineContainer}}
	for _, tc := range []struct {
		name  string
		allow []string
		want  string
	}{
		{"within same", []string{"api.files.test"}, ""},
		{"a host outside same", []string{"api.files.test", "wide.test"},
			"mcp.sandbox.policy.overlay.allowed_domains: Narrower than the run can only remove access. Edit files to remove the extra hosts."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comps := stdioComps(1)
			comps.attached[0].snapshot.Definition.MCP.Sandbox = &types.ComponentMCPSandbox{Policy: types.ComponentMCPSandboxPolicy{
				Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{AllowedDomains: &tc.allow}}}
			rec := httptest.NewRecorder()
			f := &runFold{w: rec, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil),
				req:   &createRunRequest{MachineKind: string(types.MachineContainer)},
				place: runPlacement{pooled: true, placement: types.PlacementRemote}, comps: comps,
				spec: types.RunPolicySpec{AllowedDomains: []string{"a.test"}}}
			cont := s.stepMCPSandbox(f, nil)
			if tc.want == "" {
				if !cont || rec.Code != http.StatusOK {
					t.Fatalf("refused %d %s, want admitted", rec.Code, rec.Body.String())
				}
				return
			}
			var body errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if cont || rec.Code != http.StatusUnprocessableEntity || body.Reason != reasonComponentDefinitionInvalid || body.Error != tc.want {
				t.Fatalf("fold = %v %d %q %q, want 422 %q %q", cont, rec.Code, body.Reason, body.Error, reasonComponentDefinitionInvalid, tc.want)
			}
		})
	}
}

// TestMCPAdmissionRefusesUpstreamProxy: a site whose traffic goes through an upstream proxy, by URL or by
// secret, refuses a server that starts from a command at admission, since its sandbox's proxy has no
// upstream; a remote server is admitted.
func TestMCPAdmissionRefusesUpstreamProxy(t *testing.T) {
	want := "MCP servers that start from a command can't run here yet: this organisation sends traffic through its own proxy. Use a remote (HTTPS) MCP server, or remove files."
	for _, site := range []types.SiteConfig{
		{UpstreamProxyURL: "http://proxy.corp.test:3128"},
		{UpstreamProxySecretRef: "corp-proxy"},
	} {
		site.Components = localMCPSite("on").Components
		t.Setenv(envMCPOwnSandbox, "1")
		refused := mcpAdmissionRefusal(site, stdioComps(1).attached[0], 0)
		if refused == nil || refused.reason != reasonMCPOwnSandboxUnsupported || refused.refusal.status != http.StatusUnprocessableEntity ||
			refused.refusal.body.Error != want {
			t.Fatalf("site %+v: admission = %+v, want 422 %q %q", site, refused, reasonMCPOwnSandboxUnsupported, want)
		}
		remote := attachedComponent{snapshot: types.RunComponent{Name: "Remote", Definition: types.ComponentDefinition{
			MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.remote.test/mcp"}}}}
		if refused := mcpAdmissionRefusal(site, remote, 0); refused != nil {
			t.Fatalf("a remote server is refused: %+v", refused)
		}
	}
}
