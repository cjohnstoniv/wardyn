// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func testSpec() runner.SandboxSpec {
	return runner.SandboxSpec{
		RunID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		Env:              map[string]string{"FOO": "bar"},
		ProxyConfig:      runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://cp:8080"},
		Resources:        runner.Resources{CPUMillis: 1000, MemoryMiB: 256},
	}
}

func newTestDriver(f *fakeDocker) *Driver {
	return newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
}

// TestEnsureImage_MissingHintsMakeTarget locks in the actionable error: when an
// agent image is absent locally and cannot be pulled (the demo tags live in no
// registry), the failure names the fix rather than leaking a bare
// "registry: denied".
func TestEnsureImage_MissingHintsMakeTarget(t *testing.T) {
	f := newFakeDocker()
	f.failImagePull = true // absent + unpullable
	d := newTestDriver(f)

	err := d.ensureImage(context.Background(), "wardyn/agent-oracle:demo")
	if err == nil {
		t.Fatal("ensureImage on an absent, unpullable image: got nil error")
	}
	if !strings.Contains(err.Error(), "make agent-images") {
		t.Errorf("error missing the fix hint: %v", err)
	}
	if !strings.Contains(err.Error(), "wardyn/agent-oracle:demo") {
		t.Errorf("error missing the image ref: %v", err)
	}
}

func TestCreateSandbox_TopologyPreservesL0(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.Driver != driverName {
		t.Errorf("Driver = %q", sb.Driver)
	}
	if sb.EnforcedClass != types.CC1 {
		t.Errorf("EnforcedClass = %q, want CC1", sb.EnforcedClass)
	}

	runID := testSpec().RunID
	intNet := internalNetName(runID)

	// (1) Per-run network must be Internal=true (no gateway => L0 upheld).
	opts, ok := f.networks[intNet]
	if !ok {
		t.Fatalf("internal network %q not created", intNet)
	}
	if !opts.Internal {
		t.Error("per-run network must be Internal=true so it has no gateway (L0)")
	}

	// (2) Agent must be attached ONLY to the per-run internal network at create
	// time. That network is Internal=true (gatewayless), so the agent has no
	// default route (L0). We attach via NetworkMode+NetworkingConfig rather than
	// "none"+NetworkConnect because Docker forbids connecting a "none"-mode
	// container to another network; the no-default-route guarantee is identical.
	agent := f.containers[agentContainerName(runID)]
	if agent == nil {
		t.Fatal("agent container not created")
	}
	if string(agent.host.NetworkMode) != intNet {
		t.Errorf("agent NetworkMode = %q, want %q (gatewayless internal net => no default route)", agent.host.NetworkMode, intNet)
	}
	if string(agent.host.NetworkMode) == "default" || string(agent.host.NetworkMode) == "bridge" {
		t.Error("agent must NEVER use the default/bridge network (would grant a default route, breaking L0)")
	}
	// Agent is placed on exactly the per-run internal network via NetworkingConfig
	// at create — never the control-plane network.
	if agent.net == nil || agent.net.EndpointsConfig == nil {
		t.Fatal("agent must be attached to the per-run internal network at create (NetworkingConfig)")
	}
	if _, ok := agent.net.EndpointsConfig[intNet]; !ok {
		t.Errorf("agent NetworkingConfig endpoints = %v, want key %q", agent.net.EndpointsConfig, intNet)
	}
	if _, ok := agent.net.EndpointsConfig[d.cfg.InternalNetwork]; ok {
		t.Error("agent must NEVER join the control-plane network (would break L0)")
	}
	// The agent uses no post-create NetworkConnect (the Docker 29.x restriction).
	if len(agent.connectedTo) != 0 {
		t.Errorf("agent connectedTo = %v, want [] (attached at create, not via NetworkConnect)", agent.connectedTo)
	}

	// (3) Proxy is on the per-run internal net AND the control-plane net.
	proxy := f.containers[proxyContainerName(runID)]
	if proxy == nil {
		t.Fatal("proxy container not created")
	}
	if string(proxy.host.NetworkMode) != intNet {
		t.Errorf("proxy NetworkMode = %q, want %q", proxy.host.NetworkMode, intNet)
	}
	if !containsStr(proxy.connectedTo, d.cfg.InternalNetwork) {
		t.Errorf("proxy must connect to control-plane network %q, connectedTo=%v", d.cfg.InternalNetwork, proxy.connectedTo)
	}

	// Agent hardening sanity (full matrix tested in hardening_test.go).
	if len(agent.host.CapDrop) != 1 || agent.host.CapDrop[0] != "ALL" {
		t.Errorf("agent CapDrop = %v, want [ALL]", agent.host.CapDrop)
	}

	// Proxy env carries the FULL sidecar config as one JSON var: run token,
	// control-plane URL, and the run's egress policy (a proxy without a
	// policy fails closed — the GAP-1 regression this guards against).
	var cfgJSON string
	for _, e := range proxy.cfg.Env {
		if strings.HasPrefix(e, "WARDYN_PROXY_CONFIG_JSON=") {
			cfgJSON = strings.TrimPrefix(e, "WARDYN_PROXY_CONFIG_JSON=")
		}
	}
	if cfgJSON == "" {
		t.Fatalf("proxy env missing WARDYN_PROXY_CONFIG_JSON: %v", proxy.cfg.Env)
	}
	var pcfg struct {
		RunToken        string         `json:"run_token"`
		ControlPlaneURL string         `json:"control_plane_url"`
		Policy          map[string]any `json:"policy"`
	}
	if err := json.Unmarshal([]byte(cfgJSON), &pcfg); err != nil {
		t.Fatalf("proxy config json invalid: %v", err)
	}
	if pcfg.RunToken != "tok" {
		t.Errorf("proxy config run_token = %q, want tok", pcfg.RunToken)
	}
	if pcfg.ControlPlaneURL != "http://cp:8080" {
		t.Errorf("proxy config control_plane_url = %q", pcfg.ControlPlaneURL)
	}
	if pcfg.Policy == nil {
		t.Error("proxy config missing the run's egress policy")
	}

	// No secret env leaked into the agent (invariant 1): the run token must
	// not appear in ANY agent env var, including embedded in JSON.
	for _, e := range agent.cfg.Env {
		if strings.Contains(e, "tok") && !strings.HasPrefix(e, "FOO=") {
			t.Errorf("run token must NOT appear in agent env: %s", e)
		}
	}
}

// TestCreateSandbox_TopologyPreservesL0UnderGVisor is the CC2-specific sibling
// of TestCreateSandbox_TopologyPreservesL0. It locks in that egress confinement
// under gVisor (CC2/runsc) is the SAME gatewayless-network + static-hosts
// mechanism as CC1 — never an iptables REDIRECT/NAT rule, which would
// crash-loop under gVisor's netstack (no nat table). If a future change tried
// to add a REDIRECT-based path for CC2, the Internal=true / no-default-route
// assertions here would still need to hold, and the runsc runtime pin proves
// this test actually exercised the gVisor branch (not silently CC1).
func TestCreateSandbox_TopologyPreservesL0UnderGVisor(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.info = infoWithRuntimes("runsc")
	d := newTestDriver(f)

	spec := testSpec()
	spec.ConfinementClass = types.CC2

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.EnforcedClass != types.CC2 {
		t.Fatalf("EnforcedClass = %q, want CC2 (test must exercise the gVisor branch)", sb.EnforcedClass)
	}

	runID := spec.RunID
	intNet := internalNetName(runID)

	// Same L0 topology as CC1: Internal=true (no gateway), not iptables/REDIRECT.
	opts, ok := f.networks[intNet]
	if !ok {
		t.Fatalf("internal network %q not created", intNet)
	}
	if !opts.Internal {
		t.Error("per-run network must be Internal=true under CC2 too — egress confinement is structural, not an in-sandbox iptables rule")
	}

	agent := f.containers[agentContainerName(runID)]
	if agent == nil {
		t.Fatal("agent container not created")
	}
	if agent.host.Runtime != "runsc" {
		t.Errorf("agent Runtime = %q, want runsc (CC2)", agent.host.Runtime)
	}
	if string(agent.host.NetworkMode) != intNet {
		t.Errorf("agent NetworkMode = %q, want %q (gatewayless internal net => no default route)", agent.host.NetworkMode, intNet)
	}

	// The gVisor-specific adaptation: a static ExtraHosts entry for wardyn-proxy,
	// because gVisor's netstack does not traverse Docker's embedded DNS resolver
	// (driver.go). Losing this would break the agent's only egress path under CC2.
	found := false
	for _, h := range agent.host.ExtraHosts {
		if strings.HasPrefix(h, "wardyn-proxy:") {
			found = true
		}
	}
	if !found {
		t.Errorf("agent ExtraHosts = %v, want a static wardyn-proxy:<ip> entry (gVisor does not resolve the container alias via embedded DNS)", agent.host.ExtraHosts)
	}
}

func TestCreateSandbox_FailClosedOnMissingRuntime(t *testing.T) {
	f := newFakeDocker() // runc only, no runsc
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.ConfinementClass = types.CC2 // demands gVisor

	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CC2 without runsc must fail closed, got nil")
	}
	if !errors.Is(err, errRuntimeUnavailable) {
		t.Errorf("error must wrap errRuntimeUnavailable, got %v", err)
	}
	// Nothing must have been created (fail closed before provisioning).
	if len(f.containers) != 0 || len(f.networks) != 0 {
		t.Errorf("no objects must be created on fail-closed: containers=%d networks=%d", len(f.containers), len(f.networks))
	}
}

func TestCreateSandbox_RollsBackOnAgentFailure(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.failCreateContainer = "wardyn-agent-" // agent create fails
	d := newTestDriver(f)

	_, err := d.CreateSandbox(context.Background(), testSpec())
	if err == nil {
		t.Fatal("expected agent create failure")
	}
	runID := testSpec().RunID
	// Proxy must have been removed and the network cleaned up.
	if proxy := f.containers[proxyContainerName(runID)]; proxy != nil && !proxy.removed {
		t.Error("proxy must be rolled back when agent create fails")
	}
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("internal network must be rolled back when agent create fails")
	}
}

func TestCreateSandbox_RequiresProxyImage(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{}) // no ProxyImage
	_, err := d.CreateSandbox(context.Background(), testSpec())
	if !errors.Is(err, errProxyImageUnset) {
		t.Errorf("want errProxyImageUnset, got %v", err)
	}
}

func TestTeardown_Idempotent(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// First kill tears everything down.
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	runID := testSpec().RunID
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("internal network must be removed on kill")
	}
	// Second kill on the now-gone sandbox must be a no-op (idempotent).
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Errorf("second KillSandbox must be idempotent, got %v", err)
	}
	// Stop on a gone sandbox is also idempotent.
	if err := d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Errorf("StopSandbox on gone sandbox must be nil, got %v", err)
	}
}

// ITEM 35: when the agent's wardyn.run-id label is unreadable, teardown must
// recover the run id from the deterministic agent container name and still tear
// down the sibling proxy (routable network, run token) + per-run network, rather
// than orphaning them and reporting a false success.
func TestTeardown_RecoversRunIDFromNameWhenLabelMissing(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	// Simulate a stripped/stale run-id label on the agent container.
	f.containers[sb.Ref].cfg.Labels = map[string]string{}

	if err := d.teardown(context.Background(), sb.Ref); err != nil {
		t.Fatalf("teardown must succeed via name-recovered run id, got %v", err)
	}
	if p := f.containers[proxyContainerName(runID)]; p == nil || !p.removed {
		t.Errorf("proxy sidecar must be torn down via name-recovered run id (removed=%v)", p != nil && p.removed)
	}
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("per-run network must be removed via name-recovered run id")
	}
}

// A CORRUPT (present but non-UUID) run-id label must ALSO fall back to the
// deterministic container name — not just an empty label — so parseRunID
// failing on garbage never orphans the proxy/network.
func TestTeardown_RecoversRunIDFromNameWhenLabelCorrupt(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	f.containers[sb.Ref].cfg.Labels = map[string]string{labelRun: "not-a-uuid"}

	if err := d.teardown(context.Background(), sb.Ref); err != nil {
		t.Fatalf("teardown must succeed via name-recovered run id despite a corrupt label, got %v", err)
	}
	if p := f.containers[proxyContainerName(runID)]; p == nil || !p.removed {
		t.Errorf("proxy sidecar must be torn down despite the corrupt label (removed=%v)", p != nil && p.removed)
	}
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("per-run network must be removed despite the corrupt label")
	}
}

// ITEM 35: if teardown removed the agent but can resolve NEITHER the run-id label
// NOR the run id from a deterministic name, it must report the teardown error
// honestly (sibling proxy/network may be orphaned) — never a false success.
func TestTeardown_UnresolvableRunReportsError(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// Neither a run-id label nor a recoverable wardyn-agent-<uuid> name.
	f.containers[sb.Ref].cfg.Labels = map[string]string{}
	f.containers[sb.Ref].name = "mystery-container"

	if err := d.teardown(context.Background(), sb.Ref); !errors.Is(err, errTeardownUnresolved) {
		t.Fatalf("unresolvable run id must report errTeardownUnresolved, got %v", err)
	}
}

func TestStatus_Mapping(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	st, err := d.Status(context.Background(), sb.Ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != types.RunRunning {
		t.Errorf("running container State = %q, want RUNNING", st.State)
	}

	// Gone container reports STOPPED, not an error.
	st, err = d.Status(context.Background(), "wardyn-agent-deadbeef")
	if err != nil {
		t.Fatalf("Status on missing: %v", err)
	}
	if st.State != types.RunStopped {
		t.Errorf("missing container State = %q, want STOPPED", st.State)
	}
}

func TestExec_WrapsWithRecorderWhenEnabled(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", Record: true})

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.Exec(context.Background(), sb.Ref, []string{"claude", "code"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	cmd := f.lastExecCmd
	if len(cmd) == 0 || cmd[0] != "wardyn-rec" {
		t.Fatalf("recording exec must start with wardyn-rec, got %v", cmd)
	}
	if !containsStr(cmd, "claude") || !containsStr(cmd, "code") {
		t.Errorf("agent argv must be preserved after wrapping, got %v", cmd)
	}
	// run id must be threaded through for deterministic recording filename.
	if !containsStr(cmd, testSpec().RunID.String()) {
		t.Errorf("recorder argv must carry run id, got %v", cmd)
	}
}

func TestExec_NoRecorderWhenDisabled(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f) // Record defaults false
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.Exec(context.Background(), sb.Ref, []string{"bash"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := f.lastExecCmd; len(got) != 1 || got[0] != "bash" {
		t.Errorf("argv must be unwrapped when recording disabled, got %v", got)
	}
}

// An EXEC-LESS runtime (krun) cannot docker-exec a workload into its microVM, so
// CreateSandbox must DEFER agent creation and Exec must create the container with
// the (recorder-wrapped) workload as its MAIN process; Wait then returns the
// container's own exit code. This is the CC3/Vault execution path.
func TestExecLess_MainProcessLifecycle(t *testing.T) {
	f := newFakeDocker()
	f.info = infoWithRuntimes("krun") // CC3 via krun; krun has no docker exec
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", Record: true})

	spec := testSpec()
	spec.ConfinementClass = types.CC3
	ctx := context.Background()

	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.EnforcedClass != types.CC3 {
		t.Errorf("EnforcedClass = %q, want CC3", sb.EnforcedClass)
	}
	agentName := agentContainerName(spec.RunID)
	if sb.Ref != agentName {
		t.Errorf("exec-less ref = %q, want agent name %q", sb.Ref, agentName)
	}
	// Agent container is DEFERRED — only the proxy exists at CreateSandbox.
	if _, exists := f.containers[agentName]; exists {
		t.Fatal("exec-less agent container must be deferred, but it was created at CreateSandbox")
	}

	// Exec creates the agent with the workload as its main process (no docker exec).
	if err := d.Exec(ctx, sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec (main process): %v", err)
	}
	c, ok := f.containers[agentName]
	if !ok {
		t.Fatal("Exec must create the deferred agent container")
	}
	if c.cfg == nil || len(c.cfg.Cmd) == 0 || c.cfg.Cmd[0] != "wardyn-rec" {
		t.Fatalf("main-process Cmd must be recorder-wrapped, got %v", c.cfg.Cmd)
	}
	if !containsStr(c.cfg.Cmd, "agent-run") || !containsStr(c.cfg.Cmd, "task") {
		t.Errorf("workload argv must be preserved in main-process Cmd, got %v", c.cfg.Cmd)
	}
	if len(f.lastExecCmd) != 0 {
		t.Errorf("exec-less path must NOT docker-exec, got lastExecCmd=%v", f.lastExecCmd)
	}

	// Wait returns the container's exit code (main process exit == container exit).
	f.mu.Lock()
	f.containers[agentName].state = &container.State{Status: "exited", ExitCode: 7}
	f.mu.Unlock()
	code, werr := d.Wait(ctx, sb.Ref)
	if werr != nil {
		t.Fatalf("Wait (main process): %v", werr)
	}
	if code != 7 {
		t.Errorf("Wait code = %d, want 7 (container exit)", code)
	}
}

func TestExec_RejectsEmptyArgv(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, _ := d.CreateSandbox(context.Background(), testSpec())
	if err := d.Exec(context.Background(), sb.Ref, nil); err == nil {
		t.Error("empty argv must error")
	}
}

func TestAttach_OpensInteractiveShellNotTrackedAsAgentExec(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Attach opens a fresh interactive shell exec.
	sess, err := d.Attach(context.Background(), sb.Ref, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sess.Close()

	// Attach opens the persistent interactive shell session (tmux/bash wrapper),
	// not a bare /bin/sh. The exact command is the package's attachShell.
	if got := f.lastExecCmd; !equalStr(got, attachShell) {
		t.Errorf("attach must exec the interactive shell %v, got %v", attachShell, got)
	}

	// CRITICAL: the attach exec must NOT be registered as the agent exec — that
	// map is exclusively the agent process Wait tracks. Attaching before any
	// Exec must leave Wait with nothing to observe (a separate, human stream).
	d.mu.Lock()
	_, tracked := d.agentExecs[sb.Ref]
	d.mu.Unlock()
	if tracked {
		t.Error("attach exec must NOT be registered in agentExecs (it is a human shell, not the agent process)")
	}

	// Read/Write round-trip through the fake hijack (fakeConn: read EOF, write ok).
	if _, err := sess.Write([]byte("echo hi\n")); err != nil {
		t.Errorf("Session.Write: %v", err)
	}

	// Resize must drive ContainerExecResize with rows->Height, cols->Width.
	if err := sess.Resize(context.Background(), 120, 40); err != nil {
		t.Errorf("Session.Resize: %v", err)
	}
	if f.lastResize == nil {
		t.Fatal("Resize must call ContainerExecResize")
	}
	if f.lastResize.Height != 40 || f.lastResize.Width != 120 {
		t.Errorf("Resize options = %+v, want Height=40 Width=120", *f.lastResize)
	}

	// Close must not error and must be idempotent.
	if err := sess.Close(); err != nil {
		t.Errorf("Session.Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Session.Close must be idempotent, got %v", err)
	}
}

func TestAttach_RejectsEmptyRef(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)
	if _, err := d.Attach(context.Background(), "", runner.AttachOptions{}); err == nil {
		t.Error("Attach with empty ref must error")
	}
}

func TestCapabilities_FromDaemonInfo(t *testing.T) {
	f := newFakeDocker()
	f.info = infoWithRuntimes("runsc")
	d := newTestDriver(f)
	caps, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !equalClasses(caps.Classes, []types.ConfinementClass{types.CC1, types.CC2}) {
		t.Errorf("Classes = %v", caps.Classes)
	}
}
