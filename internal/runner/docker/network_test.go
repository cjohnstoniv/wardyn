// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

// network_test.go pulls the e2e.sh egress invariants down into real Go
// integration tests for the docker driver's L0 STRUCTURAL-EGRESS guarantee
// (ARCHITECTURE.md invariant 3): a created sandbox has NO default route, the
// cloud metadata / link-local address is UNREACHABLE from it, and its sole
// egress path is the wardyn-proxy sidecar on the per-run internal network.
//
// These tests stand up a REAL sandbox against a live Docker daemon. They are
// guarded by WARDYN_TEST_DOCKER=1 and skip cleanly when it is unset, so plain
// unit-CI never needs Docker. The //go:build docker tag matches the driver's
// own build tag — the file only compiles under `-tags docker`.
//
// Substrate recipe mirrors test/conformance/conformance_docker_test.go and
// integration_test.go: busybox:latest doubles as both the agent image (it has
// busybox `ip`, `wget`, and `ping` for the probes) and the proxy image (we only
// validate L3/L4 topology here, not the real proxy's L7 behaviour). The
// pre-existing 'wardyn-internal' control-plane network is created best-effort.

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newNetworkTestDriver constructs a real-daemon driver (busybox proxy image)
// and ensures the control-plane network exists so CreateSandbox can attach the
// proxy sidecar. It fails the test if a daemon client cannot be built.
func newNetworkTestDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(Config{
		// busybox doubles as the proxy image: these tests assert the L3/L4
		// network topology, not the real proxy's L7 behaviour, so a sidecar
		// that exits quickly is sufficient — the network attachments persist.
		ProxyImage: "busybox:latest",
	})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	// The proxy joins this control-plane network at create; without it
	// CreateSandbox fails. Best-effort create (ignore "already exists").
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	return d
}

// createNetworkSandbox creates a CC1 busybox sandbox with a unique run id and
// registers teardown via t.Cleanup, returning the driver, the run id, and the
// sandbox. Using a fresh uuid per test keeps the test isolated within the
// shared daemon (no assumptions about other containers/networks present).
func createNetworkSandbox(t *testing.T, ctx context.Context) (*Driver, uuid.UUID, runner.Sandbox) {
	t.Helper()
	d := newNetworkTestDriver(t)
	runID := uuid.New()
	spec := runner.SandboxSpec{
		RunID:            runID,
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig:      runner.ProxyConfig{RunToken: "net-test", ControlPlaneURL: "http://127.0.0.1:0"},
		Resources:        runner.Resources{CPUMillis: 500, MemoryMiB: 128},
		Labels:           map[string]string{"wardyn.test": "network"},
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() {
		// KillSandbox cascades agent + proxy + per-run network removal; it is
		// idempotent, so a double-clean (e.g. a test that also kills) is fine.
		_ = d.KillSandbox(context.Background(), sb.Ref)
	})
	return d, runID, sb
}

// execInSandbox runs argv inside ref and returns the demultiplexed combined
// output (stdout+stderr). Non-TTY exec attach uses Docker's 8-byte framed
// stream protocol, so we demux with stdcopy.StdCopy — io.Copy alone would
// corrupt the text with binary frame headers (the same approach as the
// conformance dockerRouteProbe).
func execInSandbox(ctx context.Context, t *testing.T, d *Driver, ref string, argv []string) string {
	t.Helper()
	created, err := d.cli.ContainerExecCreate(ctx, ref, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          argv,
	})
	if err != nil {
		t.Fatalf("exec create %v: %v", argv, err)
	}
	resp, err := d.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach %v: %v", argv, err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader); err != nil && err != io.EOF {
		t.Logf("exec %v: stdcopy warning: %v (continuing)", argv, err)
	}
	return stdout.String() + stderr.String()
}

// TestL0_NoDefaultRoute asserts e2e.sh invariant 3 directly: a freshly created
// sandbox has NO default route. We exec `ip route` (busybox ships `ip`) and
// require that no line begins with "default" — a default route would mean
// 0.0.0.0/0 is reachable, breaking L0 structural egress. The agent is attached
// only to the per-run Internal=true network, which Docker provisions with no
// gateway, so the routing table must contain only the on-link subnet route.
func TestL0_NoDefaultRoute(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	d, _, sb := createNetworkSandbox(t, ctx)

	out := execInSandbox(ctx, t, d, sb.Ref, []string{"ip", "route"})
	if out == "" {
		t.Fatal("ip route produced no output; cannot verify L0 (expected the on-link subnet route)")
	}
	// busybox `ip route` prints the full table; assert NO default route line.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			t.Errorf("L0 violated: sandbox %q has a default route.\nip route:\n%s", sb.Ref, out)
		}
	}
	// Positive sanity: the agent DOES have an on-link interface on the per-run
	// internal network (eth0), so it can reach the proxy's segment. Without it
	// the "no default route" assertion would pass vacuously on a netless box.
	if !strings.Contains(out, "dev eth0") {
		t.Errorf("sandbox must have an on-link route via eth0 (the per-run internal net), got:\n%s", out)
	}
}

// TestL0_MetadataUnreachable asserts the cloud metadata / link-local endpoint
// (169.254.169.254 — the classic SSRF credential-theft target) is UNREACHABLE
// from the sandbox. With no default route on a gatewayless internal network,
// the kernel has no path to the link-local range, so a route lookup must fail
// with "Network is unreachable" and a connect attempt must not succeed.
func TestL0_MetadataUnreachable(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	d, _, sb := createNetworkSandbox(t, ctx)

	const metadataIP = "169.254.169.254"

	// (a) The kernel must have NO route to the metadata IP. `ip route get`
	// resolves the path; on a gatewayless segment it fails closed with
	// "unreachable". A successful lookup (a "dev"/"via" line) would mean a
	// route exists — an L0 violation.
	routeGet := execInSandbox(ctx, t, d, sb.Ref, []string{"ip", "route", "get", metadataIP})
	if !strings.Contains(strings.ToLower(routeGet), "unreachable") {
		t.Errorf("metadata IP %s must have NO route from the sandbox; `ip route get` returned:\n%s", metadataIP, routeGet)
	}

	// (b) A real connect attempt must fail (no egress path). We use wget with a
	// short timeout; success would mean the metadata service was reachable.
	// busybox wget exits non-zero and prints "Network is unreachable" / a
	// connect error. We append the shell RC so a silent success cannot pass.
	conn := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c",
		"wget -q -T 3 -O - http://" + metadataIP + "/latest/meta-data/ 2>&1; echo RC=$?",
	})
	if strings.Contains(conn, "RC=0") {
		t.Errorf("connect to metadata %s must FAIL (no egress path); wget succeeded:\n%s", metadataIP, conn)
	}
	low := strings.ToLower(conn)
	if !strings.Contains(low, "unreachable") && !strings.Contains(low, "can't connect") && !strings.Contains(low, "bad address") {
		t.Errorf("connect to metadata %s should report an unreachable/connect error, got:\n%s", metadataIP, conn)
	}
}

// TestL0_ProxyIsSoleEgressPath asserts the egress topology that makes the proxy
// the sandbox's ONLY way off the per-run segment:
//
//	(1) the agent reaches a peer (a stand-in for the proxy) on the per-run
//	    internal network — egress to that segment works; and
//	(2) the agent reaches NOTHING off that segment (the metadata IP and a
//	    public IP are both unreachable: no default route).
//
// We stand up a busybox httpd "listener" on the SAME per-run internal network
// (the real proxy exits quickly under busybox, so a live listener gives a
// deterministic TCP target). A non-error HTTP response (even 404) proves L4
// connectivity to the in-segment peer, while the off-segment probes confirm the
// agent is otherwise sealed — exactly the proxy-only egress invariant.
func TestL0_ProxyIsSoleEgressPath(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	d, runID, sb := createNetworkSandbox(t, ctx)

	// Stand up a live in-segment peer (busybox httpd) on the per-run internal
	// network, reachable by the alias "wardyn-egress-peer". This models the proxy
	// as a reachable L4 endpoint on the agent's only network.
	intNet := internalNetName(runID)
	const peerAlias = "wardyn-egress-peer"
	peerName := "wardyn-test-egress-peer-" + runID.String()
	peerResp, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image: "busybox:latest",
			// httpd -f stays in the foreground listening on :3128 (the proxy port).
			Cmd: []string{"httpd", "-f", "-p", "3128"},
		},
		&container.HostConfig{NetworkMode: container.NetworkMode(intNet)},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				intNet: {Aliases: []string{peerAlias}},
			},
		},
		nil, peerName,
	)
	if err != nil {
		t.Fatalf("create egress peer: %v", err)
	}
	t.Cleanup(func() {
		// Remove the peer FIRST (LIFO: this runs before createNetworkSandbox's
		// KillSandbox cleanup). Then best-effort remove the per-run network with a
		// short retry: a lingering peer endpoint can otherwise leave the network
		// behind because Docker refuses to remove a net with active endpoints, and
		// force-remove's endpoint detach is not strictly synchronous.
		cctx := context.Background()
		_ = d.cli.ContainerRemove(cctx, peerResp.ID, container.RemoveOptions{Force: true})
		for i := 0; i < 20; i++ {
			if err := d.cli.NetworkRemove(cctx, intNet); err == nil || isNotFound(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	if err := d.cli.ContainerStart(ctx, peerResp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start egress peer: %v", err)
	}
	// Give httpd a moment to bind before the agent probes it.
	waitListening(ctx, t, d, sb.Ref, peerAlias, 3128)

	// (1) Egress to the in-segment peer WORKS: a connect that completes the TCP
	// handshake and gets an HTTP reply (404 is fine — busybox httpd has no
	// docroot) proves L4 reachability on the per-run net. A "Network is
	// unreachable"/"can't connect" here would mean the agent cannot even reach
	// its own segment, which would be a regression.
	peer := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c",
		"wget -q -T 5 -O - http://" + peerAlias + ":3128/ 2>&1; echo RC=$?",
	})
	low := strings.ToLower(peer)
	if strings.Contains(low, "unreachable") || strings.Contains(low, "can't connect") || strings.Contains(low, "bad address") {
		t.Errorf("agent must reach the in-segment proxy peer on the per-run net, got:\n%s", peer)
	}

	// (2a) Off-segment metadata is still UNREACHABLE (no default route).
	md := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c",
		"wget -q -T 3 -O - http://169.254.169.254/ 2>&1; echo RC=$?",
	})
	if strings.Contains(md, "RC=0") {
		t.Errorf("agent must NOT reach the metadata IP off-segment; got:\n%s", md)
	}

	// (2b) A public IP is also UNREACHABLE (the agent has no path off-host; only
	// the proxy bridges to the control-plane network). Probe by IP so the result
	// reflects routing, not DNS resolution.
	pub := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c",
		"wget -q -T 3 -O - http://1.1.1.1/ 2>&1; echo RC=$?",
	})
	if strings.Contains(pub, "RC=0") {
		t.Errorf("agent must NOT reach a public IP directly (proxy is the only egress); got:\n%s", pub)
	}
}

// TestL0_ProxyOnlyDualHomedBridge asserts the structural topology that backs
// the proxy-only egress invariant, read straight from the daemon via inspect:
//
//   - the AGENT is attached to EXACTLY the per-run internal network and is NOT
//     on the control-plane ('wardyn-internal') network — it has no path that
//     bridges off the per-run segment; and
//   - the PROXY is dual-homed: it is on BOTH the per-run internal network (the
//     agent's segment) AND the control-plane network — it is the single bridge.
//
// Inspecting the network endpoints is robust even though the busybox proxy
// process exits quickly: Docker keeps the network attachments on the container
// record regardless of whether its main process is still running.
func TestL0_ProxyOnlyDualHomedBridge(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	d, runID, sb := createNetworkSandbox(t, ctx)
	intNet := internalNetName(runID)

	// Agent: on the per-run internal net ONLY; never the control-plane net.
	agent, err := d.cli.ContainerInspect(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("inspect agent: %v", err)
	}
	agentNets := networkNames(agent)
	if !containsStr(agentNets, intNet) {
		t.Errorf("agent must be attached to the per-run internal net %q, got %v", intNet, agentNets)
	}
	if containsStr(agentNets, d.cfg.InternalNetwork) {
		t.Errorf("agent must NEVER join the control-plane net %q (would grant an off-segment bridge, breaking L0); got %v", d.cfg.InternalNetwork, agentNets)
	}
	if len(agentNets) != 1 {
		t.Errorf("agent must be attached to EXACTLY the per-run internal net; got %v", agentNets)
	}
	// The agent's per-run endpoint must have NO gateway (Internal=true network),
	// which is the structural reason it has no default route.
	if gw := agent.NetworkSettings.Networks[intNet].Gateway; gw != "" {
		t.Errorf("per-run internal net must be gatewayless (Internal=true) so the agent has no default route; got gateway %q", gw)
	}

	// Proxy: dual-homed across the per-run net AND the control-plane net.
	proxy, err := d.cli.ContainerInspect(ctx, proxyContainerName(runID))
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	proxyNets := networkNames(proxy)
	if !containsStr(proxyNets, intNet) {
		t.Errorf("proxy must be on the per-run internal net %q (shared with the agent), got %v", intNet, proxyNets)
	}
	if !containsStr(proxyNets, d.cfg.InternalNetwork) {
		t.Errorf("proxy must be on the control-plane net %q (the single bridge off-segment), got %v", d.cfg.InternalNetwork, proxyNets)
	}
}

// waitListening polls (bounded) until the agent can complete a TCP connect to
// host:port on its own segment, so the egress-path assertions do not race the
// busybox httpd startup. It never fails the test — the assertions that follow
// own the verdict; this only reduces flakiness.
func waitListening(ctx context.Context, t *testing.T, d *Driver, ref, host string, port int) {
	t.Helper()
	probe := []string{"sh", "-c", "wget -q -T 1 -O - http://" + host + ":" + strconv.Itoa(port) + "/ 2>&1; echo RC=$?"}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := strings.ToLower(execInSandbox(ctx, t, d, ref, probe))
		// Any reply that is NOT a routing/connect failure means the listener is up.
		if !strings.Contains(out, "unreachable") && !strings.Contains(out, "can't connect") && !strings.Contains(out, "refused") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// networkNames returns the sorted set of network names a container is attached
// to, read from its inspect result. Returns nil for a container with no
// NetworkSettings (should not happen for a created container).
func networkNames(insp container.InspectResponse) []string {
	if insp.NetworkSettings == nil {
		return nil
	}
	names := make([]string, 0, len(insp.NetworkSettings.Networks))
	for name := range insp.NetworkSettings.Networks {
		names = append(names, name)
	}
	return names
}
