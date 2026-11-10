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
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

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
		// network topology, not the real proxy's L7 behaviour. Keep the sidecar
		// alive with `sleep infinity` (mirroring conformance_docker_test.go):
		// busybox's default `sh` exits immediately, and a modern dockerd releases
		// an exited container's endpoint IP, so CreateSandbox would then fail
		// resolving the proxy's per-run IP. A real wardyn-proxy is long-running.
		ProxyImage: "busybox:latest",
		ProxyCmd:   []string{"sleep", "infinity"},
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
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          argv,
	})
	if err != nil {
		t.Fatalf("exec create %v: %v", argv, err)
	}
	attachRes, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach %v: %v", argv, err)
	}
	resp := attachRes.HijackedResponse
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
	// `ip route` is IPv4 only: an IPv6 default route would be a second way off the segment. The
	// table may be empty (no IPv6 at all), which is fine; a "default" line is not.
	out6 := execInSandbox(ctx, t, d, sb.Ref, []string{"ip", "-6", "route"})
	for _, line := range strings.Split(out6, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			t.Errorf("L0 violated: sandbox %q has an IPv6 default route.\nip -6 route:\n%s", sb.Ref, out6)
		}
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

	// (c) The same endpoint over IPv6 (the EC2 IPv6 metadata address): no route, and no connect. A
	// sandbox with no IPv6 at all also answers "unreachable" or "not supported", which is the
	// sealed outcome.
	const metadataIP6 = "fd00:ec2::254"
	routeGet6 := strings.ToLower(execInSandbox(ctx, t, d, sb.Ref, []string{"ip", "-6", "route", "get", metadataIP6}))
	if !strings.Contains(routeGet6, "unreachable") && !strings.Contains(routeGet6, "not supported") {
		t.Errorf("metadata IP %s must have NO route from the sandbox; `ip -6 route get` returned:\n%s", metadataIP6, routeGet6)
	}
	conn6 := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c",
		"wget -q -T 3 -O - 'http://[" + metadataIP6 + "]/latest/meta-data/' 2>&1; echo RC=$?",
	})
	if strings.Contains(conn6, "RC=0") {
		t.Errorf("connect to metadata %s must FAIL (no egress path); wget succeeded:\n%s", metadataIP6, conn6)
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
	peerResp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "busybox:latest",
			// httpd -f stays in the foreground listening on :3128 (the proxy port).
			Cmd: []string{"httpd", "-f", "-p", "3128"},
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(intNet)},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				intNet: {Aliases: []string{peerAlias}},
			},
		},
		Name: peerName,
	})
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
		_, _ = d.cli.ContainerRemove(cctx, peerResp.ID, client.ContainerRemoveOptions{Force: true})
		for i := 0; i < 20; i++ {
			if _, err := d.cli.NetworkRemove(cctx, intNet, client.NetworkRemoveOptions{}); err == nil || isNotFound(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	if _, err := d.cli.ContainerStart(ctx, peerResp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start egress peer: %v", err)
	}
	// Give httpd a moment to bind before the agent probes it.
	waitListening(ctx, t, d, sb.Ref, peerAlias, 3128)

	// (1) Egress to the in-segment peer WORKS: a connect that completes the TCP
	// handshake and gets an HTTP reply (404 is fine — busybox httpd has no
	// docroot) proves L4 reachability on the per-run net. A "Network is
	// unreachable"/"can't connect" here would mean the agent cannot even reach
	// its own segment.
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
	agentRes, err := d.cli.ContainerInspect(ctx, sb.Ref, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect agent: %v", err)
	}
	agent := agentRes.Container
	agentNets := networkNames(agent)
	if !slices.Contains(agentNets, intNet) {
		t.Errorf("agent must be attached to the per-run internal net %q, got %v", intNet, agentNets)
	}
	if slices.Contains(agentNets, d.cfg.InternalNetwork) {
		t.Errorf("agent must NEVER join the control-plane net %q (would grant an off-segment bridge, breaking L0); got %v", d.cfg.InternalNetwork, agentNets)
	}
	if len(agentNets) != 1 {
		t.Errorf("agent must be attached to EXACTLY the per-run internal net; got %v", agentNets)
	}
	// The agent's per-run endpoint must have NO gateway (Internal=true network),
	// which is the structural reason it has no default route. v29 types the
	// Gateway as a netip.Addr, so a valid Addr means a gateway is present.
	if gw := agent.NetworkSettings.Networks[intNet].Gateway; gw.IsValid() {
		t.Errorf("per-run internal net must be gatewayless (Internal=true) so the agent has no default route; got gateway %q", gw)
	}

	// Proxy: dual-homed across the per-run net AND the control-plane net.
	proxyRes, err := d.cli.ContainerInspect(ctx, proxyContainerName(runID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	proxy := proxyRes.Container
	proxyNets := networkNames(proxy)
	if !slices.Contains(proxyNets, intNet) {
		t.Errorf("proxy must be on the per-run internal net %q (shared with the agent), got %v", intNet, proxyNets)
	}
	if !slices.Contains(proxyNets, d.cfg.InternalNetwork) {
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

// TestL0_NoDNSExfil is the settling probe, and the only thing that can
// settle it: whether a CC1 agent — sealed on a gatewayless Internal=true
// network, with no default route and no path to any resolver of its own — can
// still get an EXTERNAL name RESOLVED through Docker's embedded DNS at
// 127.0.0.11, which forwards what it cannot answer to the HOST's resolvers. If
// it can, the sandbox has a low-bandwidth channel off the box that L0's routing
// story does not cover: the query label carries the payload, and no answer has
// to come back for the data to have left.
//
// The proxy half is the constraint on any fix, and it is what makes this
// testable rather than theoretical: the agent reaches wardyn-proxy through an
// /etc/hosts entry the driver pins at create (driver.go's ExtraHosts, on EVERY
// class, not only gVisor), so pointing the sandbox at a dead nameserver would
// NOT cost it its egress path. Both halves are asserted together so a future
// fix cannot silence the second by breaking the first.
//
// Scope: CC1 only. Under gVisor (CC2) and a microVM (CC3) the netstack does not
// traverse Docker's embedded resolver at all.
func TestL0_NoDNSExfil(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// The sandbox's OWN proxy sidecar is the listener, under the alias and IP the
	// driver pins into the agent's /etc/hosts — a second container aliased
	// wardyn-proxy would collide with it rather than stand in for it.
	d, err := New(Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"httpd", "-f", "-p", "3128"}})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig:      runner.ProxyConfig{RunToken: "dns-test", ControlPlaneURL: "http://127.0.0.1:0"},
		Resources:        runner.Resources{CPUMillis: 500, MemoryMiB: 128},
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	waitListening(ctx, t, d, sb.Ref, "wardyn-proxy", 3128)

	// (1) The agent's one egress path resolves and connects.
	proxy := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c", "wget -q -T 5 -O - http://wardyn-proxy:3128/ 2>&1; echo RC=$?",
	})
	// The negative-substring check below is this file's house pattern, and on its
	// own an exec that returned NOTHING passes it. The probe appends its own shell
	// RC, so requiring that marker is what makes an empty read a failure.
	if !strings.Contains(proxy, "RC=") {
		t.Fatalf("the proxy probe produced no RC marker — the exec returned nothing, so the check below proves nothing:\n%s", proxy)
	}
	low := strings.ToLower(proxy)
	if strings.Contains(low, "bad address") || strings.Contains(low, "unreachable") || strings.Contains(low, "refused") {
		t.Fatalf("the agent must resolve and reach wardyn-proxy:3128 (its only egress path), got:\n%s", proxy)
	}

	// (2) The agent's only nameserver is its run proxy (the driver mounts a resolv.conf naming it), so a
	// lookup goes to the proxy's port 53, not to Docker's embedded resolver. The proxy here is busybox httpd,
	// which does not answer on 53, so the lookup fails by timing out; TestL0_DNSQueryIsRecorded runs the same
	// path against the real proxy image and reads the row. Non-vacuous: the file must name the proxy's address,
	// the one the agent's hosts file pins, so a sandbox that silently kept Docker's resolver cannot pass.
	const exfilName = "wardyn-l0-dns-exfil-probe.example.com"
	resolv := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "cat /etc/resolv.conf"})
	proxyIP := strings.TrimSpace(execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "awk '$2==\"wardyn-proxy\"{print $1}' /etc/hosts"}))
	if proxyIP == "" || strings.TrimSpace(resolv) != "nameserver "+proxyIP {
		t.Fatalf("the agent's /etc/resolv.conf is %q, want exactly %q (its run proxy and no other nameserver)", resolv, "nameserver "+proxyIP)
	}
	ext := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c", "nslookup -type=a " + exfilName + " 2>&1; echo RC=$?",
	})
	if !strings.Contains(ext, "RC=") || strings.Contains(ext, "127.0.0.11") {
		t.Fatalf("the external-name probe printed no exit code, or still went to Docker's embedded resolver:\n%s", ext)
	}
	if strings.Contains(ext, "RC=0") {
		t.Errorf("L0 DNS exfil: an external name resolved from inside a sealed CC1 sandbox through its run proxy:\n%s", ext)
	}

	// (3) The escape this test was written for stays shut: the agent asking Docker's embedded resolver at
	// 127.0.0.11 directly. It forwards what it cannot answer to the HOST's resolvers, which a gatewayless
	// Internal=true network leaves it nowhere to send. busybox nslookup exits non-zero when the resolver
	// refuses or cannot answer; a resolved record means the query reached the host's resolvers and, with it,
	// whatever the label carried. Non-vacuous: the resolver has to have ANSWERED for its refusal to mean
	// anything. It names itself in the reply (Server: 127.0.0.11), and what it answers with is SERVFAIL, not
	// NXDOMAIN: it tried to forward upstream and had nowhere to send it, which is the L0 topology doing the
	// work rather than a DNS policy.
	direct := execInSandbox(ctx, t, d, sb.Ref, []string{
		"sh", "-c", "nslookup -type=a " + exfilName + " 127.0.0.11 2>&1; echo RC=$?",
	})
	if !strings.Contains(direct, "127.0.0.11") {
		t.Fatalf("the probe of Docker's embedded resolver never reached it, so its failure proves nothing:\n%s", direct)
	}
	// Raw output of each probe, so `go test -v` is an evidence record of WHAT the sandbox saw and not merely
	// of the exit code. This finding is settled by observation.
	t.Logf("proxy probe (the agent's only egress path):\n%s", proxy)
	t.Logf("resolv.conf:\n%s", resolv)
	t.Logf("external-name probe through the run proxy (%s):\n%s", exfilName, ext)
	t.Logf("external-name probe at the embedded resolver (%s):\n%s", exfilName, direct)
	if strings.Contains(direct, "RC=0") {
		t.Errorf("L0 DNS exfil: an external name resolved from inside a sealed CC1 sandbox — "+
			"Docker's embedded resolver forwarded the query to the host's resolvers, so the query "+
			"label is a channel off the box:\n%s", direct)
	}
}

// TestL0_HostAddressUnreachable: a run's network has no address on the Docker host, so its agent
// cannot reach a service the host serves on every address (a daemon bound to 0.0.0.0, a published
// port). The same run shows the premise: a container on a network created the old way (Internal=true
// alone) reaches that service at its bridge's host address. The run's real paths still work: the
// agent reaches its proxy, and the proxy reaches the host over the control-plane network, the way a
// runner's relay is reached (host.docker.internal is host-gateway there).
//
// The bridge's addresses are read from this host, so it needs the daemon on the machine running the
// test, as in CI; elsewhere it skips, and the l0-docker suite's REQUIRE_ALL turns that skip red.
func TestL0_HostAddressUnreachable(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The host's service, on every IPv4 address of this host.
	const marker = "wardyn-host-listener"
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen on the host: %v", err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, marker)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	// The run, with a proxy that listens on its port (busybox httpd), as TestL0_NoDNSExfil has it.
	d, err := New(Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"httpd", "-f", "-p", "3128"}})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	fetch := func(ref, host string) string {
		return execInSandbox(ctx, t, d, ref, []string{"sh", "-c", "wget -q -T 5 -O - http://" + host + ":" + port + "/ 2>&1; echo RC=$?"})
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	runID := uuid.New()
	sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            runID,
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig:      runner.ProxyConfig{RunToken: "host-test", ControlPlaneURL: "http://127.0.0.1:0"},
		Resources:        runner.Resources{CPUMillis: 500, MemoryMiB: 128},
		Labels:           map[string]string{"wardyn.test": "network"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })

	// (1) Positive control: a network created the old way, and a container on it.
	oldNet := "wardyn-test-old-shape-" + runID.String()
	oldRes, err := d.cli.NetworkCreate(ctx, oldNet, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{"wardyn.test": "network"}})
	if err != nil {
		t.Fatalf("create the old-shape network: %v", err)
	}
	oldProbe := "wardyn-test-old-shape-probe-" + runID.String()
	probeRes, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"sleep", "300"}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(oldNet)},
		Name:       oldProbe,
	})
	if err != nil {
		t.Fatalf("create the old-shape probe: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = d.cli.ContainerRemove(cctx, probeRes.ID, client.ContainerRemoveOptions{Force: true})
		for range 20 {
			if _, err := d.cli.NetworkRemove(cctx, oldRes.ID, client.NetworkRemoveOptions{}); err == nil || isNotFound(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	if _, err := d.cli.ContainerStart(ctx, probeRes.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start the old-shape probe: %v", err)
	}
	oldV4, _, found := hostBridgeAddrs(t, oldRes.ID)
	if !found {
		t.Skipf("no bridge br-%s on this host: the Docker daemon is not on the machine running the test", oldRes.ID[:12])
	}
	if len(oldV4) == 0 {
		t.Fatalf("premise: the old-shape network's bridge br-%s has no IPv4 address on this host, so the old shape was not reachable here", oldRes.ID[:12])
	}
	old := fetch(oldProbe, oldV4[0].String())
	if !strings.Contains(old, marker) {
		t.Fatalf("positive control: a container on the old-shape network did not reach the host's service at %s:%s, so the check below would prove nothing:\n%s", oldV4[0], port, old)
	}
	t.Logf("old shape: a container on %s reached the host's service at its bridge address %s:%s", oldNet, oldV4[0], port)

	// (2) The run's network: created isolated with IPv6 off, no host address on its bridge, and the
	// address the bridge would otherwise hold does not reach the host's service.
	insp, err := d.cli.NetworkInspect(ctx, internalNetName(runID), client.NetworkInspectOptions{})
	if err != nil {
		t.Fatalf("inspect the run's network: %v", err)
	}
	if problem := runNetworkShapeProblem(insp.Network); problem != "" {
		t.Errorf("the run's network %s; want gateway mode isolated and IPv6 off", problem)
	}
	runV4, runV6, found := hostBridgeAddrs(t, insp.Network.ID)
	if !found {
		t.Fatalf("no bridge br-%s for the run's network on this host, though the old shape's was found", insp.Network.ID[:12])
	}
	if len(runV4) != 0 {
		t.Errorf("the run's network's bridge has the host address(es) %v; want none", runV4)
	}
	var subnet netip.Prefix
	for _, c := range insp.Network.IPAM.Config {
		if c.Subnet.Addr().Is4() {
			subnet = c.Subnet
		}
	}
	if !subnet.IsValid() {
		t.Fatalf("the run's network has no IPv4 subnet: %+v", insp.Network.IPAM)
	}
	wouldBe := subnet.Masked().Addr().Next() // the address Docker gives the bridge on the old shape
	got := fetch(sb.Ref, wouldBe.String())
	if !strings.Contains(got, "RC=") {
		t.Fatalf("the agent's probe of %s returned nothing:\n%s", wouldBe, got)
	}
	if strings.Contains(got, marker) {
		t.Errorf("the agent reached the host's service at %s:%s on its own network:\n%s", wouldBe, port, got)
	}
	t.Logf("run network: the agent's probe of %s:%s got:\n%s", wouldBe, port, got)

	// (3) No IPv6 path either: when the bridge holds an IPv6 address on the host, the agent must have
	// no IPv6 address of its own to reach it from.
	if len(runV6) != 0 {
		agent6 := execInSandbox(ctx, t, d, sb.Ref, []string{"ip", "-6", "addr"})
		for _, line := range strings.Split(agent6, "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "inet6" && f[1] != "::1/128" {
				t.Errorf("the run's bridge holds %v on the host and the agent has the IPv6 address %s, a path to the host:\n%s", runV6, f[1], agent6)
			}
		}
	}

	// (4) The agent still reaches its proxy.
	waitListening(ctx, t, d, sb.Ref, "wardyn-proxy", 3128)
	px := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "wget -q -T 5 -O - http://wardyn-proxy:3128/ 2>&1; echo RC=$?"})
	if low := strings.ToLower(px); !strings.Contains(px, "RC=") || strings.Contains(low, "bad address") || strings.Contains(low, "unreachable") ||
		strings.Contains(low, "refused") || strings.Contains(low, "can't connect") {
		t.Errorf("the agent must reach wardyn-proxy:3128, its only egress path, got:\n%s", px)
	}

	// (5) The proxy still reaches the host over the control-plane network.
	if up := fetch(proxyContainerName(runID), "host.docker.internal"); !strings.Contains(up, marker) {
		t.Errorf("the proxy must reach the host's service at host.docker.internal:%s over the control-plane network, got:\n%s", port, up)
	}
}

// hostBridgeAddrs reads this host's addresses on the bridge Docker made for network id
// (br-<first 12 of the id>): its IPv4 addresses, its IPv6 ones, and whether the bridge is here at all.
func hostBridgeAddrs(t *testing.T, id string) (v4, v6 []netip.Addr, found bool) {
	t.Helper()
	iface, err := net.InterfaceByName("br-" + id[:12])
	if err != nil {
		return nil, nil, false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		t.Fatalf("addresses of %s: %v", iface.Name, err)
	}
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		if p.Addr().Is4() {
			v4 = append(v4, p.Addr())
		} else {
			v6 = append(v6, p.Addr())
		}
	}
	return v4, v6, true
}
