// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The values the dispatch tests seed: the server's own env secret and header key, and the agent's own secret.
const (
	mcpFilesSecretValue = "files-env-secret-value-4f1c9a"
	mcpFilesKeyValue    = "files-header-key-value-7d20be"
	mcpAgentSecretValue = "agent-env-secret-value-93ab6e"
)

// mcpDispatchStore is dispatch's store with the run's component snapshot, its MCP rows, its owner's walls
// (whose read fails while wallsErr is set), its kept proxy config and, for a restart, a run read that fails
// while getRunErr is set and a containment held while pending is.
type mcpDispatchStore struct {
	*dispatchTestStore
	*mcpRowsMem
	store.ComponentStore
	comps     []types.RunComponent
	wallsErr  error
	getRunErr error
	pending   bool
	cfgMu     sync.Mutex
	proxyCfg  []byte
	putErr    func() error // when set, PutRunProxyConfig fails with what it answers
	site      types.SiteConfig
}

func (s *mcpDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

// The restart and revive paths find these by assertion, so a signature drift would read as "not kept".
var (
	_ store.RunProxyConfigs    = (*mcpDispatchStore)(nil)
	_ store.RunMCPSandboxStore = (*mcpDispatchStore)(nil)
	_ runContainmentStore      = (*mcpDispatchStore)(nil)
)

func (s *mcpDispatchStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.getRunErr != nil {
		return types.AgentRun{}, s.getRunErr
	}
	return s.dispatchTestStore.GetRun(ctx, id)
}

func (s *mcpDispatchStore) PutRunProxyConfig(_ context.Context, _ uuid.UUID, sealed []byte) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.putErr != nil {
		if err := s.putErr(); err != nil {
			return err
		}
	}
	s.proxyCfg = slices.Clone(sealed)
	return nil
}

func (s *mcpDispatchStore) GetRunProxyConfig(context.Context, uuid.UUID) ([]byte, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.proxyCfg == nil {
		return nil, store.ErrNotFound
	}
	return slices.Clone(s.proxyCfg), nil
}

func (s *mcpDispatchStore) DeleteRunProxyConfig(context.Context, uuid.UUID) error { return nil }

func (s *mcpDispatchStore) PurgeTerminalRunProxyConfigs(context.Context) (int64, error) {
	return 0, nil
}

func (s *mcpDispatchStore) GetRunContainmentPending(context.Context, uuid.UUID) (store.RunContainmentPendingRow, error) {
	if s.pending {
		return store.RunContainmentPendingRow{}, nil
	}
	return store.RunContainmentPendingRow{}, store.ErrNotFound
}

func (s *mcpDispatchStore) ListRunNodeRecords(context.Context, uuid.UUID) ([]types.RunNodeRecord, error) {
	return nil, nil
}

func (s *mcpDispatchStore) QueueRunNodeRecord(context.Context, store.RunNodeRecordWrite) (types.RunnerPendingAction, error) {
	return types.RunnerPendingAction{}, errors.New("not kept")
}

func (s *mcpDispatchStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.comps, nil
}

func (s *mcpDispatchStore) GetPrincipalPref(context.Context, string, string) (json.RawMessage, error) {
	if s.wallsErr != nil {
		return nil, s.wallsErr
	}
	return nil, store.ErrNotFound
}

func (s *mcpDispatchStore) PutPrincipalPref(context.Context, string, string, json.RawMessage) error {
	return errors.New("not kept")
}

// mcpDispatchRunner is the agent's fake runner with MCP sandboxes in memory, advertising container ones.
type mcpDispatchRunner struct {
	*fakeRunner
	*mcpSandboxerFake
}

func (r mcpDispatchRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	c, err := r.fakeRunner.Capabilities(ctx)
	c.MCPSandboxKinds = []types.MachineKind{types.MachineContainer}
	return c, err
}

// mcpFilesDef is a server that starts from a command with its config, an env secret and a header key.
func mcpFilesDef() types.ComponentDefinition {
	return types.ComponentDefinition{
		Hosts:  []string{"api.files.test"},
		Config: map[string]string{"FILES_ROOT": "/data"},
		Secrets: []types.ComponentSecret{
			{SecretName: "files-token", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "FILES_TOKEN"}},
			{SecretName: "files-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.files.test"}},
		},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
			Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}},
	}
}

type mcpDispatchFixture struct {
	srv   *Server
	st    *mcpDispatchStore
	fr    *fakeRunner
	sbx   *mcpSandboxerFake
	audit *syncAudit
	run   types.AgentRun
	p     dispatchParams
	keyID uuid.UUID // the header key's grant
}

// newMCPDispatchFixture is a run with one server that starts from a command, and the agent's own env secret,
// with its dispatch params built as the gate builds them (runComponents.dispatch). extra are more snapshot
// rows, after it in ordinal order.
func newMCPDispatchFixture(t *testing.T, extra ...types.RunComponent) *mcpDispatchFixture {
	t.Helper()
	t.Setenv(envMCPOwnSandbox, "on")
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Image: "wardyn/claude-code:latest",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "docker", Task: "do the thing"}
	keyID := uuid.New()
	// The header key's grant row, as the run's create writes it: the take matches an injection by its row.
	base := &dispatchTestStore{run: run, state: types.RunPending,
		grants: []types.CredentialGrant{{ID: keyID, RunID: run.ID, Spec: componentGrant(mcpFilesDef().Secrets[1])}}}
	rows := &mcpRowsMem{clock: now.Truncate(time.Second)}
	rows.runOf = func(id uuid.UUID) (types.AgentRun, error) { return base.GetRun(context.Background(), id) }
	rc := types.RunComponent{Ordinal: 0, Name: "Files", Definition: mcpFilesDef(), MCPHarnesses: []string{"claude-code"}}
	st := &mcpDispatchStore{dispatchTestStore: base, mcpRowsMem: rows, comps: append([]types.RunComponent{rc}, extra...)}
	secrets := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	for name, v := range map[string]string{"files-token": mcpFilesSecretValue, "files-key": mcpFilesKeyValue, "agent-token": mcpAgentSecretValue} {
		if err := secrets.For(run.CreatedBy).Put(context.Background(), name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	fr, sbx, audit := &fakeRunner{}, newMCPSandboxerFake(), &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = mcpDispatchRunner{fr, sbx}
	cfg.Broker, cfg.Audit, cfg.Secrets = h.broker, audit, secrets
	cfg.RunConfigKey = bytes.Repeat([]byte{7}, kek.DEKSize) // the run's and each MCP sandbox's config are kept
	// The watchers a dispatch starts end with the test, so none reads the fixture after it.
	every := mcpWatchEvery
	mcpWatchEvery = time.Hour // a test runs each pass itself
	t.Cleanup(func() { mcpWatchEvery = every })
	life, cancel := context.WithCancel(context.Background())
	cfg.BaseCtx = life
	t.Cleanup(cancel)

	def := rc.Definition
	comps := runComponents{attached: []attachedComponent{{snapshot: rc}}}
	for _, e := range extra {
		comps.attached = append(comps.attached, attachedComponent{snapshot: e})
	}
	f := &mcpDispatchFixture{srv: New(cfg), st: st, fr: fr, sbx: sbx, audit: audit, run: run, keyID: keyID}
	f.p.RunToken, f.p.Image = "run-token", "wardyn/claude-code:latest"
	f.p.Policy.MinConfinementClass = types.CC1
	f.p.Policy.EligibleGrants = []types.GrantSpec{
		componentGrant(def.Secrets[0]), componentGrant(def.Secrets[1]),
		{Kind: types.GrantEnvSecret, OwnerOnly: true, Scope: mustJSON(map[string]string{"name": "AGENT_TOKEN", "secret_name": "agent-token"})},
	}
	f.p.Injections = []runner.InjectionGrant{{GrantID: f.keyID, Rule: egress.InjectionRule{Host: "api.files.test", SecretName: "files-key", RequireTLS: true}}}
	f.p.Components = comps.dispatch()
	return f
}

func (f *mcpDispatchFixture) dispatch() {
	f.srv.dispatchRun(context.Background(), f.run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), f.p)
}

func (f *mcpDispatchFixture) agentSpec() runner.SandboxSpec {
	f.fr.mu.Lock()
	defer f.fr.mu.Unlock()
	return f.fr.lastSpec
}

// assertRowsEnded: every row stopped by the dispatch's own end, with its end row, and no sandbox left.
func (f *mcpDispatchFixture) assertRowsEnded(t *testing.T) {
	t.Helper()
	for _, r := range f.st.mcpRowsMem.all() {
		if r.State != store.MCPSandboxStopped || r.LastCause != mcpEndUnstarted {
			t.Errorf("row %s = %s (%q), want stopped with cause %q", r.ServerKey, r.State, r.LastCause, mcpEndUnstarted)
		}
	}
	if len(f.st.mcpRowsMem.all()) == 0 || !f.audit.has(f.run.ID, "run.mcp_sandbox.end", "success") {
		t.Errorf("rows %d, run.mcp_sandbox.end success %v", len(f.st.mcpRowsMem.all()), f.audit.has(f.run.ID, "run.mcp_sandbox.end", "success"))
	}
	if f.sbx.left() != 0 {
		t.Errorf("%d MCP sandboxes left", f.sbx.left())
	}
}

func (f *mcpDispatchFixture) state() types.RunState {
	r, _ := f.st.dispatchTestStore.GetRun(context.Background(), f.run.ID)
	return r.State
}

// TestMCPSandboxSpecHoldsNoRunCredentials (I5 a, b): the server's sandbox gets its own config and secrets, a
// platform env inside the closed list, and a proxy config under an MCP-audience token of its own; nothing of
// the agent's (its token, its secret) reaches the spec.
func TestMCPSandboxSpecHoldsNoRunCredentials(t *testing.T) {
	f := newMCPDispatchFixture(t)
	f.dispatch()
	if len(f.sbx.specs) != 1 {
		t.Fatalf("MCP sandboxes created = %d, want 1 (run %s)", len(f.sbx.specs), f.state())
	}
	sp := f.sbx.specs[0]
	if err := sp.Validate(); err != nil {
		t.Errorf("spec refused by its own check: %v", err)
	}
	if sp.Image != f.p.Image || sp.ServerKey == "" || sp.RelayRank != 0 || sp.Launch.Command != "npx" {
		t.Errorf("spec = image %q key %q rank %d launch %+v", sp.Image, sp.ServerKey, sp.RelayRank, sp.Launch)
	}
	if sp.Env["FILES_ROOT"] != "/data" || len(sp.Env) != 1 || sp.SecretEnv["FILES_TOKEN"] != mcpFilesSecretValue || len(sp.SecretEnv) != 1 {
		t.Errorf("env %v secret env %v: want only the server's own", sp.Env, sp.SecretEnv)
	}
	var pc proxy.Config
	if err := json.Unmarshal(sp.ProxyConfig, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.ControlPlaneAudience != mcpSandboxAudience || pc.RunToken == "" || pc.RunToken == f.p.RunToken {
		t.Errorf("proxy token audience %q, token is the agent's: %v", pc.ControlPlaneAudience, pc.RunToken == f.p.RunToken)
	}
	if len(pc.Injection) != 1 || pc.Injection[0].GrantID != f.keyID || !slices.Equal(pc.MITMHosts, []string{"api.files.test:443"}) ||
		pc.MCPRelayInbound == nil || pc.MCPRelayInbound.TokenSHA256 != sp.RelayTokenSHA256 || !slices.Contains(pc.RelayScrubValues, mcpFilesSecretValue) {
		t.Errorf("proxy config injection %+v mitm %v relay %+v", pc.Injection, pc.MITMHosts, pc.MCPRelayInbound)
	}
	all := string(sp.ProxyConfig) + string(mustJSON(sp.Env)) + string(mustJSON(sp.SecretEnv)) + string(mustJSON(sp.PlatformEnv))
	for _, leak := range []string{mcpAgentSecretValue, f.p.RunToken + `"`, "AGENT_TOKEN"} {
		if strings.Contains(all, leak) {
			t.Errorf("the MCP sandbox's spec holds %q", leak)
		}
	}
	row := f.st.mcpRowsMem.get(0)
	if !slices.Equal(row.GrantIDs, []uuid.UUID{f.keyID}) || row.RelayTokenSHA256 != sp.RelayTokenSHA256 || row.RelayAddr == "" {
		t.Errorf("row = %+v", row)
	}
}

// TestAgentSpecHoldsNoMCPSandboxSecret (I5 c): the agent's sandbox and proxy hold none of the server's config or
// secrets (no variable, no value, no header injection, no interception entry), while the agent keeps its own
// secret; its proxy config reaches the server through the relay alone.
func TestAgentSpecHoldsNoMCPSandboxSecret(t *testing.T) {
	f := newMCPDispatchFixture(t)
	if slices.Contains(f.p.Components.MITMHosts, "api.files.test:443") || slices.Contains(f.p.Components.HeaderHosts, "api.files.test") ||
		len(f.p.Components.Config) != 0 {
		t.Fatalf("components dispatch %+v: the server's header host or config reached the agent's half", f.p.Components)
	}
	f.dispatch()
	spec := f.agentSpec()
	if spec.Image == "" {
		t.Fatalf("no agent sandbox created (run %s)", f.state())
	}
	if spec.Env["AGENT_TOKEN"] != mcpAgentSecretValue {
		t.Errorf("the agent's own secret is missing: %q", spec.Env["AGENT_TOKEN"])
	}
	for _, name := range []string{"FILES_TOKEN", "FILES_ROOT"} {
		if _, ok := spec.Env[name]; ok {
			t.Errorf("agent env holds %s", name)
		}
		if _, ok := spec.SecretEnv[name]; ok {
			t.Errorf("agent secret env holds %s", name)
		}
	}
	if leak := mcpAgentSpecLeak(spec, []mcpServer{{key: "files"}}, []mcpOwn{{secretEnv: map[string]string{"FILES_TOKEN": ""},
		values: []string{mcpFilesSecretValue, mcpFilesKeyValue}}}); leak != "" {
		t.Errorf("agent spec: %s", leak)
	}
	pc := spec.ProxyConfig
	if slices.ContainsFunc(pc.Injection, func(in runner.InjectionGrant) bool { return in.Rule.SecretName == "files-key" }) ||
		slices.Contains(pc.MITMHosts, "api.files.test:443") {
		t.Errorf("agent proxy injection %+v mitm %v: the server's header key", pc.Injection, pc.MITMHosts)
	}
	if slices.ContainsFunc(pc.Policy.EligibleGrants, func(g types.GrantSpec) bool { return strings.Contains(string(g.Scope), "files-") }) {
		t.Errorf("agent proxy policy keeps a grant of the server's: %+v", pc.Policy.EligibleGrants)
	}
	i := slices.IndexFunc(pc.MCPServers, func(m proxy.MCPServerConfig) bool { return m.URL == "" })
	if i < 0 || pc.MCPServers[i].Relay == nil || pc.MCPServers[i].Relay.Addr != "172.30.0.2:3130" || pc.MCPServers[i].Relay.Token == "" {
		t.Fatalf("agent proxy MCP servers %+v: want the server on its relay", pc.MCPServers)
	}
	if strings.Contains(string(mustJSON(pc)), mcpFilesSecretValue) {
		t.Error("the agent's proxy config holds the server's secret value")
	}
}

// TestRefusedAtAdmissionStartsNoMCPServer (I8): a run its last admission refuses, and a run whose
// recording cannot start, each see the server created and killed and never started.
func TestRefusedAtAdmissionStartsNoMCPServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*mcpDispatchFixture)
	}{
		{"admission", func(f *mcpDispatchFixture) {
			f.p.MemberDrives = map[uuid.UUID]string{uuid.New(): "someone-else@example.com"}
		}},
		{"recording", func(f *mcpDispatchFixture) { f.st.wallsErr = errors.New("walls unreadable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPDispatchFixture(t)
			tc.set(f)
			f.dispatch()
			if f.sbx.count("create") != 1 || f.sbx.count("kill") == 0 || f.sbx.count("start") != 0 || f.sbx.left() != 0 {
				t.Fatalf("calls %v, left %d: want created, killed, never started", f.sbx.calls, f.sbx.left())
			}
			if f.agentSpec().Image != "" || f.state() != types.RunFailed {
				t.Errorf("agent created %v, run %s", f.agentSpec().Image != "", f.state())
			}
			f.assertRowsEnded(t)
		})
	}
}

// TestCreateFailureLeavesNoMCPSandbox: a create that fails fails the run before the agent's sandbox is
// created, kills what it may have left and fails its row with the cause; a host the sandbox would be
// reachable from is refused with its own reason.
func TestCreateFailureLeavesNoMCPSandbox(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cause string
	}{
		{errors.New("docker: no such image"), mcpFailCreate},
		{runner.ErrMCPSandboxHostReachable, reasonMCPSandboxHostReachable},
	} {
		f := newMCPDispatchFixture(t)
		f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error { return tc.err }
		f.dispatch()
		if f.sbx.left() != 0 || f.sbx.count("start") != 0 || f.agentSpec().Image != "" || f.state() != types.RunFailed {
			t.Fatalf("%v: left %d calls %v agent %v run %s", tc.err, f.sbx.left(), f.sbx.calls, f.agentSpec().Image != "", f.state())
		}
		if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxFailed || row.LastCause != tc.cause {
			t.Errorf("%v: row %s cause %q, want failed %q", tc.err, row.State, row.LastCause, tc.cause)
		}
		if !f.audit.has(f.run.ID, "run.mcp_sandbox.create", "failure") {
			t.Errorf("%v: no run.mcp_sandbox.create failure row", tc.err)
		}
	}
}

// TestDispatchReturnLeavesNoMCPSandbox (I8): every return of a dispatch after the servers were created and
// before the run is RUNNING ends them: the final proxy config not kept, the agent's create failing, and the
// run killed between the servers' start and its move to RUNNING (the lost CAS). The admission and recording
// returns are TestRefusedAtAdmissionStartsNoMCPServer's.
func TestDispatchReturnLeavesNoMCPSandbox(t *testing.T) {
	for name, set := range map[string]func(*mcpDispatchFixture){
		"final config not kept": func(f *mcpDispatchFixture) {
			f.st.putErr = func() error {
				if f.sbx.count("create") > 0 {
					return errors.New("store down")
				}
				return nil
			}
		},
		"agent create fails": func(f *mcpDispatchFixture) { f.fr.createErr = errors.New("driver refused") },
		"killed before RUNNING": func(f *mcpDispatchFixture) {
			f.sbx.statusFn = func() {
				f.st.dispatchTestStore.mu.Lock()
				f.st.dispatchTestStore.state = types.RunKilled
				f.st.dispatchTestStore.mu.Unlock()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPDispatchFixture(t)
			set(f)
			f.dispatch()
			if f.sbx.count("create") != 1 || f.sbx.left() != 0 || f.state() == types.RunRunning {
				t.Fatalf("calls %v left %d run %s: want the server created, then ended, and the run not RUNNING", f.sbx.calls, f.sbx.left(), f.state())
			}
			f.assertRowsEnded(t)
		})
	}
}

// TestKillDuringMCPCreateLeavesNoSandbox (I8): a kill's own row end, or a person's Stop, that runs between
// the row's insert and its create finds no sandbox to kill and stops the row; the create then makes one. The
// dispatch's refused record kills it, so no sandbox, the agent's or the server's, is left, and the row never
// moves to ready.
func TestKillDuringMCPCreateLeavesNoSandbox(t *testing.T) {
	for name, end := range map[string]func(*mcpDispatchFixture){
		"kill": func(f *mcpDispatchFixture) {
			f.st.dispatchTestStore.mu.Lock()
			f.st.dispatchTestStore.state = types.RunKilled
			f.st.dispatchTestStore.mu.Unlock()
			f.srv.endRunMCPSandboxes(context.Background(), f.run.ID, mcpEndKilled)
		},
		"stop": func(f *mcpDispatchFixture) {
			if w := f.mcpRouteCall(t, f.srv.handleStopRunMCPServer, http.MethodPost, "files", "stop"); w.Code != http.StatusOK {
				t.Errorf("stop during the create = %d %s", w.Code, w.Body.String())
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPDispatchFixture(t)
			f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error {
				end(f)
				return nil
			}
			f.dispatch()
			if f.sbx.count("create") != 1 || f.sbx.left() != 0 || f.agentSpec().Image != "" {
				t.Fatalf("left %d calls %v agent %v", f.sbx.left(), f.sbx.calls, f.agentSpec().Image != "")
			}
			if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxStopped {
				t.Errorf("row %s (%q), want stopped by the end that raced the create", row.State, row.LastCause)
			}
		})
	}
}

// TestMCPTakeHonoursOwnerOnly: an agent grant for a server's secret that does not carry the server's own
// owner-only rule, for its env secret and for its header key, is taken off the agent and given to no one:
// the server gets no value its own grant would not resolve, and the agent keeps neither.
func TestMCPTakeHonoursOwnerOnly(t *testing.T) {
	f := newMCPDispatchFixture(t)
	def := mcpFilesDef()
	env, key := componentGrant(def.Secrets[0]), componentGrant(def.Secrets[1])
	env.OwnerOnly, key.OwnerOnly = false, false
	f.p.Policy.EligibleGrants = []types.GrantSpec{env, f.p.Policy.EligibleGrants[2]}
	f.st.dispatchTestStore.grants = []types.CredentialGrant{{ID: f.keyID, RunID: f.run.ID, Spec: key}}
	f.dispatch()
	if len(f.sbx.specs) != 1 {
		t.Fatalf("MCP sandboxes created = %d, want 1 (run %s)", len(f.sbx.specs), f.state())
	}
	sp := f.sbx.specs[0]
	var pc proxy.Config
	if err := json.Unmarshal(sp.ProxyConfig, &pc); err != nil {
		t.Fatal(err)
	}
	if _, ok := sp.SecretEnv["FILES_TOKEN"]; ok || len(pc.Injection) != 0 || len(f.st.mcpRowsMem.get(0).GrantIDs) != 0 {
		t.Errorf("server secret env %v injection %+v grants %v: want neither grant given to it", sp.SecretEnv, pc.Injection, f.st.mcpRowsMem.get(0).GrantIDs)
	}
	agent := f.agentSpec()
	_, inEnv := agent.Env["FILES_TOKEN"]
	if _, ok := agent.SecretEnv["FILES_TOKEN"]; ok || inEnv ||
		slices.ContainsFunc(agent.ProxyConfig.Injection, func(in runner.InjectionGrant) bool { return in.Rule.SecretName == "files-key" }) {
		t.Errorf("the agent kept a grant of the server's secret: env %v injections %+v", agent.SecretEnv, agent.ProxyConfig.Injection)
	}
}

// TestMCPSecretResolveRowsNameTheServer: the env secret resolve row of a server's own secret carries
// mcp_sandbox, its key, and the agent's own secret's row carries none, so a run's trail tells them apart.
func TestMCPSecretResolveRowsNameTheServer(t *testing.T) {
	f := newMCPDispatchFixture(t)
	f.dispatch()
	got := map[string]any{}
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.env_secret.resolve") {
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		if name, _ := data["secret_name"].(string); name != "" {
			got[name] = data["mcp_sandbox"]
		}
	}
	if got["files-token"] != "files" {
		t.Errorf("the server's secret row names %v, want mcp_sandbox files (rows %v)", got["files-token"], got)
	}
	if v, ok := got["agent-token"]; !ok || v != nil {
		t.Errorf("the agent's secret row: present %v, mcp_sandbox %v; want present and unlabelled", ok, v)
	}
}

// TestDispatchMCPReadinessWaitEndsWithItsContext: a dispatch whose context ends during the readiness wait
// stops polling at once, rather than read the servers' status hot until the wait's deadline, and kills what
// did not come up.
func TestDispatchMCPReadinessWaitEndsWithItsContext(t *testing.T) {
	f := newMCPDispatchFixture(t)
	f.sbx.statusDead = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sbx.statusFn = cancel
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.dispatchRun(ctx, f.run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), f.p)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the dispatch did not return after its context ended")
	}
	if n := f.sbx.count("status"); n > 2 || f.sbx.left() != 0 {
		t.Errorf("status reads %d, left %d: want the wait ended at once and the server killed", n, f.sbx.left())
	}
}

// TestDispatchMCPReadinessLapseKeepsRun: a server that never comes up is failed alone with cause
// readiness and killed; the run goes on.
func TestDispatchMCPReadinessLapseKeepsRun(t *testing.T) {
	f := newMCPDispatchFixture(t)
	f.sbx.statusDead = true
	wait, poll := mcpReadyWait, mcpReadyPoll
	mcpReadyWait, mcpReadyPoll = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { mcpReadyWait, mcpReadyPoll = wait, poll })
	f.dispatch()
	if f.agentSpec().Image == "" {
		t.Fatalf("the run stopped on a server's lapse (run %s)", f.state())
	}
	if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxFailed || row.LastCause != mcpFailReadiness {
		t.Errorf("row %s cause %q, want failed readiness", row.State, row.LastCause)
	}
	if f.sbx.left() != 0 {
		t.Errorf("the lapsed server's sandbox is left")
	}
}
