// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpTestConfig is an MCP proxy's config as the control plane renders it: the listeners' hosts are the driver's
// to bind, their ports are kept.
const mcpTestConfig = `{"run_id":"11111111-1111-1111-1111-111111111111","control_plane_url":"http://127.0.0.1:8080",` +
	`"run_token":"tok","control_plane_audience":"wardyn-mcp-sandbox","listen":"0.0.0.0:3128",` +
	`"mcp_relay_inbound":{"listen":"0.0.0.0:9443","token_sha256":"ab","upstream":"0.0.0.0:8765"}}`

func mcpTestSpec(ord int, class types.ConfinementClass) runner.MCPSandboxSpec {
	return runner.MCPSandboxSpec{
		ID:          runner.MCPSandboxID{RunID: testSpec().RunID, Ordinal: ord, Class: class},
		ServerKey:   "files",
		Image:       "busybox:latest",
		Env:         map[string]string{"SERVER_MODE": "read"},
		SecretEnv:   map[string]string{"SERVER_TOKEN": "s3cret"},
		PlatformEnv: map[string]string{"WARDYN_MCP_LAUNCH_JSON": `{"command":"files-server"}`},
		ProxyConfig: []byte(mcpTestConfig),
	}
}

func newMCPRig(t *testing.T) (*fakeDocker, *Driver) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.images["wardyn-proxy:dev"] = true
	return f, newTestDriver(f)
}

func createMCP(t *testing.T, d *Driver, ord int) runner.MCPSandbox {
	t.Helper()
	sb, err := d.CreateMCPSandbox(context.Background(), mcpTestSpec(ord, types.CC1))
	if err != nil {
		t.Fatalf("CreateMCPSandbox(%d): %v", ord, err)
	}
	return sb
}

// pinnedOn is the address the container was created with, or joined at, on the network.
func pinnedOn(t *testing.T, f *fakeDocker, name, network string) netip.Addr {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[name]
	if c == nil {
		t.Fatalf("no container %s", name)
	}
	if ip, ok := c.connectedIPs[network]; ok {
		return ip
	}
	if ep := c.net.EndpointsConfig[network]; ep != nil && ep.IPAMConfig != nil {
		return ep.IPAMConfig.IPv4Address
	}
	t.Fatalf("%s has no pinned address on %s", name, network)
	return netip.Addr{}
}

func mcpStarted(f *fakeDocker) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.startedNames), func(n string) bool { return !strings.HasPrefix(n, runner.MCPSandboxNamePrefix) })
}

func gone(f *fakeDocker, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[name]
	return c == nil || c.removed
}

func hasNet(f *fakeDocker, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.networks[name]
	return ok
}

func onNet(f *fakeDocker, ctr, network string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[ctr]
	return c != nil && !c.removed && c.onNet(network)
}

func TestMCPSandboxNamesShareThePrefix(t *testing.T) {
	id := testSpec().RunID
	for _, n := range []string{mcpNetName(id, 2), mcpProxyName(id, 2), mcpContainerName(id, 2)} {
		if !strings.HasPrefix(n, runner.MCPSandboxNamePrefix) {
			t.Errorf("%s does not start with %s", n, runner.MCPSandboxNamePrefix)
		}
		if ord, ok := mcpOrdinalOf("/"+n, id); !ok || ord != 2 {
			t.Errorf("mcpOrdinalOf(%s) = %d, %v; want 2, true", n, ord, ok)
		}
	}
	for _, n := range []string{agentContainerName(id), relayLinkName(id), mcpProxyName(uuid.New(), 2), "wardyn-mcp-" + id.String() + "-+2"} {
		if _, ok := mcpOrdinalOf(n, id); ok {
			t.Errorf("mcpOrdinalOf(%s) took it for an MCP object of the run", n)
		}
	}
}

// TestCreateMCPSandboxPullsBothImages: with neither image present, the create pulls the run's image and the proxy
// image, says so for each before its pull, and starts nothing.
func TestCreateMCPSandboxPullsBothImages(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)
	var mu sync.Mutex
	var pulls, waits []string
	f.onPull = func(ref string) { mu.Lock(); pulls = append(pulls, ref); mu.Unlock() }
	spec := mcpTestSpec(0, types.CC1)
	spec.OnWaiting = func(detail string) { mu.Lock(); waits = append(waits, detail); mu.Unlock() }
	if _, err := d.CreateMCPSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateMCPSandbox: %v", err)
	}
	if want := []string{"busybox:latest", "wardyn-proxy:dev"}; !slices.Equal(pulls, want) {
		t.Errorf("pulls = %q, want %q", pulls, want)
	}
	if want := []string{"image: Pulling: busybox:latest", "image: Pulling: wardyn-proxy:dev"}; !slices.Equal(waits, want) {
		t.Errorf("waiting reports = %q, want %q", waits, want)
	}
	if s := mcpStarted(f); len(s) != 0 {
		t.Errorf("the create started %v", s)
	}
}

// TestCreateMCPSandboxStartsNothing: the create leaves the MCP proxy and the container created, not started, the
// proxy with no config written yet, and the host program copied into the container root-owned before any start.
func TestCreateMCPSandboxStartsNothing(t *testing.T) {
	f, d := newMCPRig(t)
	id := testSpec().RunID
	sb := createMCP(t, d, 0)
	if s := mcpStarted(f); len(s) != 0 {
		t.Fatalf("the create started %v", s)
	}
	for _, n := range []string{mcpProxyName(id, 0), mcpContainerName(id, 0)} {
		if c := f.containers[n]; c == nil || c.state == nil || c.state.Running || c.state.Status != "created" {
			t.Errorf("%s = %+v; want created and not running", n, c)
		}
	}
	if b := f.stdinOf(mcpProxyName(id, 0)); len(b) != 0 {
		t.Errorf("the MCP proxy was handed its config at create: %s", b)
	}
	if _, port, _ := net.SplitHostPort(sb.RelayAddr); port != "9443" {
		t.Errorf("relay address %q; want the config's relay port 9443", sb.RelayAddr)
	}
	var host *fakeCopy
	for _, c := range f.copies {
		if c.id == mcpContainerName(id, 0) && c.dest == "/etc" {
			host = &c
		}
	}
	if host == nil {
		t.Fatalf("no copy of the host program into the MCP container: %+v", f.copies)
	}
	if host.afterStart || host.copyUIDGID {
		t.Errorf("host program copy afterStart=%v copyUIDGID=%v; want before any start and root-owned", host.afterStart, host.copyUIDGID)
	}
	tr := tar.NewReader(bytes.NewReader(host.archive))
	var got []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got = append(got, h.Name+" "+strconv.FormatInt(h.Mode, 8)+" "+strconv.Itoa(h.Uid)+" "+string(body))
	}
	if want := []string{"wardyn-mcp/ 755 0 ", "wardyn-mcp/wardyn-mcp-host 555 0 " + fakeMCPHost}; !slices.Equal(got, want) {
		t.Errorf("host program archive = %q, want %q", got, want)
	}
}

// TestMCPHostBinaryOutsideManagedFileDir: the host program lands apart from every managed-file directory, and an
// image that already has its directory is refused, with everything the create made removed but the relay link.
func TestMCPHostBinaryOutsideManagedFileDir(t *testing.T) {
	for _, dir := range []string{runner.ManagedFileDir, runner.CodexManagedFileDir, runner.ComponentSecretDir} {
		if strings.HasPrefix(runner.MCPHostPath, dir+"/") {
			t.Errorf("%s is under the managed-file directory %s", runner.MCPHostPath, dir)
		}
	}
	f, d := newMCPRig(t)
	id := testSpec().RunID
	f.existingPaths = map[string]bool{path.Dir(runner.MCPHostPath): true}
	if _, err := d.CreateMCPSandbox(context.Background(), mcpTestSpec(0, types.CC1)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateMCPSandbox into an image that has %s = %v; want a refusal", path.Dir(runner.MCPHostPath), err)
	}
	if !gone(f, mcpProxyName(id, 0)) || !gone(f, mcpContainerName(id, 0)) || hasNet(f, mcpNetName(id, 0)) {
		t.Error("a refused create left an MCP object behind")
	}
	if !hasNet(f, relayLinkName(id)) {
		t.Error("a refused create removed the relay link, which lives as long as the run")
	}
}

// TestMCPSandboxHostConfigHasNoHostMounts pins I1 and I2 under each runtime: no bind and no volume, the one tmpfs,
// no device but krun's /dev/kvm, unprivileged with every capability dropped, no published port on the container
// or its proxy, and no docker socket in either.
func TestMCPSandboxHostConfigHasNoHostMounts(t *testing.T) {
	for _, tc := range []struct {
		class types.ConfinementClass
		krun  bool
	}{{types.CC1, false}, {types.CC2, false}, {types.CC3, true}} {
		t.Run(string(tc.class), func(t *testing.T) {
			f, d := newMCPRig(t)
			f.info = infoWithRuntimes("runsc", "krun")
			id := testSpec().RunID
			if _, err := d.CreateMCPSandbox(context.Background(), mcpTestSpec(0, tc.class)); err != nil {
				t.Fatalf("CreateMCPSandbox: %v", err)
			}
			c, p := f.containers[mcpContainerName(id, 0)], f.containers[mcpProxyName(id, 0)]
			h := c.host
			if len(h.Mounts) != 1 || h.Mounts[0].Type != mount.TypeTmpfs || h.Mounts[0].Target != "/tmp" || h.Mounts[0].Source != "" {
				t.Errorf("container mounts = %+v; want exactly the one tmpfs at /tmp", h.Mounts)
			}
			wantDevices := 0
			if tc.krun {
				wantDevices = 1
			}
			if len(h.Devices) != wantDevices || tc.krun && h.Devices[0].PathOnHost != "/dev/kvm" {
				t.Errorf("container devices = %+v; want %d (krun's /dev/kvm only)", h.Devices, wantDevices)
			}
			if h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) != 0 {
				t.Errorf("container privileged=%v capdrop=%v capadd=%v; want unprivileged with ALL dropped", h.Privileged, h.CapDrop, h.CapAdd)
			}
			if len(p.host.Mounts) != 0 || p.host.Privileged || !slices.Equal(p.host.CapDrop, []string{"ALL"}) {
				t.Errorf("MCP proxy mounts=%+v privileged=%v capdrop=%v", p.host.Mounts, p.host.Privileged, p.host.CapDrop)
			}
			for name, ctr := range map[string]*createdContainer{"container": c, "MCP proxy": p} {
				hc := ctr.host
				if len(hc.Binds) != 0 || len(hc.PortBindings) != 0 || hc.PublishAllPorts || len(hc.VolumesFrom) != 0 {
					t.Errorf("%s binds=%v ports=%v publishAll=%v volumesFrom=%v; want none", name, hc.Binds, hc.PortBindings, hc.PublishAllPorts, hc.VolumesFrom)
				}
				var seen []string
				seen = append(seen, ctr.cfg.Env...)
				seen = append(seen, hc.Binds...)
				for _, m := range hc.Mounts {
					seen = append(seen, m.Source, m.Target)
				}
				for _, s := range seen {
					for _, bad := range []string{"docker.sock", "DOCKER_HOST", "/run/containerd"} {
						if strings.Contains(s, bad) {
							t.Errorf("%s carries %q (%s)", name, bad, s)
						}
					}
				}
			}
		})
	}
}

// TestMCPSandboxNetworksAreExactlyThese pins I3's networks: the container on its own internal network only, the
// MCP proxy on that network, the control-plane-facing network and the relay link, and the agent on neither the
// relay link nor an MCP network while its proxy is on the link.
func TestMCPSandboxNetworksAreExactlyThese(t *testing.T) {
	f, d := newMCPRig(t)
	id := testSpec().RunID
	createMCP(t, d, 0)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	netName, link := mcpNetName(id, 0), relayLinkName(id)
	c := f.containers[mcpContainerName(id, 0)]
	if string(c.host.NetworkMode) != netName || len(c.net.EndpointsConfig) != 1 || c.net.EndpointsConfig[netName] == nil || len(c.connectedTo) != 0 {
		t.Errorf("MCP container networks: mode %s endpoints %v joined %v; want %s only", c.host.NetworkMode, c.net.EndpointsConfig, c.connectedTo, netName)
	}
	p := f.containers[mcpProxyName(id, 0)]
	if string(p.host.NetworkMode) != netName || !slices.Equal(p.connectedTo, []string{"wardyn-internal", link}) {
		t.Errorf("MCP proxy networks: mode %s joined %v; want %s, then wardyn-internal and %s", p.host.NetworkMode, p.connectedTo, netName, link)
	}
	for _, n := range []string{netName, link} {
		if !f.networks[n].Internal {
			t.Errorf("network %s is not internal", n)
		}
	}
	if onNet(f, sb.Ref, link) || onNet(f, sb.Ref, netName) {
		t.Error("the agent container is on the relay link or an MCP network")
	}
	if !onNet(f, proxyContainerName(id), link) || onNet(f, proxyContainerName(id), netName) {
		t.Error("the agent's proxy must be on the relay link and on no MCP network")
	}
}

// TestMCPSandboxDNSIsItsProxy: the container's one resolver, its hosts entry and its proxy variables name its
// proxy's MCP network address, the address the proxy's DNS listener binds.
func TestMCPSandboxDNSIsItsProxy(t *testing.T) {
	f, d := newMCPRig(t)
	id := testSpec().RunID
	createMCP(t, d, 0)
	proxyIP := pinnedOn(t, f, mcpProxyName(id, 0), mcpNetName(id, 0))
	c := f.containers[mcpContainerName(id, 0)]
	if !slices.Equal(c.host.DNS, []netip.Addr{proxyIP}) {
		t.Errorf("container DNS = %v; want only its proxy %s", c.host.DNS, proxyIP)
	}
	if !slices.Equal(c.host.ExtraHosts, []string{"wardyn-mcp-proxy:" + proxyIP.String()}) {
		t.Errorf("container hosts = %v", c.host.ExtraHosts)
	}
	if want := proxy.DNSListenEnv + "=" + net.JoinHostPort(proxyIP.String(), "53"); !slices.Contains(f.containers[mcpProxyName(id, 0)].cfg.Env, want) {
		t.Errorf("MCP proxy env %v lacks %s", f.containers[mcpProxyName(id, 0)].cfg.Env, want)
	}
	if !slices.Contains(c.cfg.Env, "HTTPS_PROXY=http://wardyn-mcp-proxy:"+strconv.Itoa(runner.ProxyListenPort)) {
		t.Errorf("container env %v does not send its traffic to its proxy", c.cfg.Env)
	}
	if got := c.cfg.Entrypoint; !slices.Equal(got, []string{runner.MCPHostPath}) {
		t.Errorf("container entrypoint = %v", got)
	}
}

// TestMCPSandboxStatusLiveOnlyWithBoth: started, the sandbox is live with its relay address; once its proxy is
// stopped (a cut), it reads dead though the container runs.
func TestMCPSandboxStatusLiveOnlyWithBoth(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	sb := createMCP(t, d, 0)
	if err := d.StartMCPSandbox(ctx, sb.ID); err != nil {
		t.Fatalf("StartMCPSandbox: %v", err)
	}
	st, err := d.MCPSandboxStatus(ctx, sb.ID)
	if err != nil || !st.Live || st.RelayAddr != sb.RelayAddr {
		t.Fatalf("status after start = %+v, %v; want live at %s", st, err, sb.RelayAddr)
	}
	if err := d.StartMCPSandbox(ctx, sb.ID); err == nil {
		t.Error("a second start was accepted; the proxy's config was written once")
	}
	if err := d.StopMCPProxies(ctx, agentContainerName(sb.ID.RunID)); err != nil {
		t.Fatalf("StopMCPProxies: %v", err)
	}
	if st, err := d.MCPSandboxStatus(ctx, sb.ID); err != nil || st.Live {
		t.Errorf("status after the proxy stopped = %+v, %v; want dead", st, err)
	}
	if gone(f, mcpContainerName(sb.ID.RunID, 0)) {
		t.Error("stopping the MCP proxies removed the MCP container")
	}
}

// recordingDocker logs the stops, kills and removes a fake engine is asked for, in order.
type recordingDocker struct {
	*fakeDocker
	log *[]string
	mu  *sync.Mutex
}

func (r recordingDocker) note(s string) { r.mu.Lock(); *r.log = append(*r.log, s); r.mu.Unlock() }

func (r recordingDocker) ContainerStop(ctx context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	t := -1
	if o.Timeout != nil {
		t = *o.Timeout
	}
	r.note("stop " + id + " " + strconv.Itoa(t))
	return r.fakeDocker.ContainerStop(ctx, id, o)
}

func (r recordingDocker) ContainerKill(ctx context.Context, id string, o client.ContainerKillOptions) (client.ContainerKillResult, error) {
	r.note("kill " + id)
	return r.fakeDocker.ContainerKill(ctx, id, o)
}

func (r recordingDocker) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	r.note("remove " + id)
	return r.fakeDocker.ContainerRemove(ctx, id, o)
}

type drainLogSink struct {
	*fakeSink
	r recordingDocker
}

func (s drainLogSink) Drain(context.Context, uuid.UUID) error { s.r.note("drain"); return nil }

// TestStopMCPProxiesDrainsThenRemoves: the run's sink is drained first, then each MCP proxy is stopped with the
// stop timeout before any kill or remove, and the MCP containers stay.
func TestStopMCPProxiesDrainsThenRemoves(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"], f.images["wardyn-proxy:dev"] = true, true
	var log []string
	r := recordingDocker{fakeDocker: f, log: &log, mu: &sync.Mutex{}}
	d := newWithClient(r, Config{ProxyImage: "wardyn-proxy:dev"})
	id := testSpec().RunID
	createMCP(t, d, 0)
	createMCP(t, d, 1)
	d.cfg.RunnerID, d.cfg.DecisionSink = "r1", drainLogSink{fakeSink: newFakeSink(), r: r}
	log = nil
	if err := d.StopMCPProxies(context.Background(), agentContainerName(id)); err != nil {
		t.Fatalf("StopMCPProxies: %v", err)
	}
	timeout := strconv.Itoa(int(stopTimeout.Seconds()))
	want := []string{"drain",
		"stop " + mcpProxyName(id, 0) + " " + timeout, "remove " + mcpProxyName(id, 0),
		"stop " + mcpProxyName(id, 1) + " " + timeout, "remove " + mcpProxyName(id, 1)}
	if !slices.Equal(log, want) {
		t.Errorf("calls = %q\nwant %q", log, want)
	}
	for _, ord := range []int{0, 1} {
		if gone(f, mcpContainerName(id, ord)) {
			t.Errorf("MCP container %d was removed by a proxy stop", ord)
		}
	}
}

// TestRestartKeepsRelayLink: a kill then a create of the server gives it the same relay address on the same
// link, which the kill left in place with the agent's proxy still on it.
func TestRestartKeepsRelayLink(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	var linkCreates int
	f.onNetworkCreate = func(name string, _ client.NetworkCreateOptions) {
		if name == relayLinkName(id) {
			linkCreates++
		}
	}
	first := createMCP(t, d, 0)
	if _, err := d.CreateSandbox(ctx, testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	subnet := f.subnets[relayLinkName(id)]
	if err := d.KillMCPSandbox(ctx, first.ID); err != nil {
		t.Fatalf("KillMCPSandbox: %v", err)
	}
	if !gone(f, mcpProxyName(id, 0)) || !gone(f, mcpContainerName(id, 0)) || hasNet(f, mcpNetName(id, 0)) {
		t.Error("the kill left an MCP object")
	}
	if !hasNet(f, relayLinkName(id)) || !onNet(f, proxyContainerName(id), relayLinkName(id)) {
		t.Fatal("the kill removed the relay link, or took the agent's proxy off it")
	}
	again := createMCP(t, d, 0)
	if again.RelayAddr != first.RelayAddr || f.subnets[relayLinkName(id)] != subnet {
		t.Errorf("re-created relay address %s on %s; want %s on %s", again.RelayAddr, f.subnets[relayLinkName(id)], first.RelayAddr, subnet)
	}
	if linkCreates != 2 { // the subnet probe and the create, once
		t.Errorf("relay link created %d times (probe and create count 2); want it made once and reused", linkCreates)
	}
}

// TestReviveKeepsRelayLink: a replaced agent proxy joins the relay link again, so the servers stay reachable.
func TestReviveKeepsRelayLink(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	createMCP(t, d, 0)
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	old := f.containers[proxyContainerName(id)]
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if p := f.containers[proxyContainerName(id)]; p == old || !onNet(f, proxyContainerName(id), relayLinkName(id)) {
		t.Error("the replaced agent proxy is not on the relay link")
	}
	if onNet(f, sb.Ref, relayLinkName(id)) {
		t.Error("the agent container joined the relay link")
	}
}

// TestKillRunMCPSandboxesSparesAgentAndLink: the run's every MCP object goes, found by listing; the agent, its
// proxy, the run network and the relay link stay, the agent's proxy still on the link.
func TestKillRunMCPSandboxesSparesAgentAndLink(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	createMCP(t, d, 0)
	createMCP(t, d, 1)
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.KillRunMCPSandboxes(ctx, id); err != nil {
		t.Fatalf("KillRunMCPSandboxes: %v", err)
	}
	for _, ord := range []int{0, 1} {
		if !gone(f, mcpProxyName(id, ord)) || !gone(f, mcpContainerName(id, ord)) || hasNet(f, mcpNetName(id, ord)) {
			t.Errorf("MCP sandbox %d survived", ord)
		}
	}
	if gone(f, sb.Ref) || gone(f, proxyContainerName(id)) || !hasNet(f, internalNetName(id)) || !hasNet(f, relayLinkName(id)) {
		t.Error("the kill reached an agent object or the relay link")
	}
	if !onNet(f, proxyContainerName(id), relayLinkName(id)) {
		t.Error("the agent's proxy left the relay link")
	}
}

// TestTeardownRemovesMCPSandboxes: the run's teardown removes its MCP sandboxes and its relay link with the rest.
func TestTeardownRemovesMCPSandboxes(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	createMCP(t, d, 0)
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if !gone(f, mcpProxyName(id, 0)) || !gone(f, mcpContainerName(id, 0)) || hasNet(f, mcpNetName(id, 0)) || hasNet(f, relayLinkName(id)) {
		t.Error("teardown left an MCP object or the relay link")
	}
}

// TestSweepRemovesOrphanMCPSandboxes: an orphan run's MCP sandbox, keyed on its container, is torn down with its
// relay link; an orphan's MCP network and relay link with no container left are swept as networks.
func TestSweepRemovesOrphanMCPSandboxes(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	createMCP(t, d, 0)
	old := time.Now().Add(-time.Hour)
	f.listItems = []container.Summary{{ID: mcpContainerName(id, 0), Created: old.Unix(),
		Labels: d.labels(id, componentMCP, nil)}}
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	for _, n := range []struct{ name, comp string }{{mcpNetName(other, 4), componentMCPNetwork}, {relayLinkName(other), componentRelayNetwork}} {
		f.networks[n.name] = client.NetworkCreateOptions{Internal: true, Labels: d.labels(other, n.comp, nil)}
		f.netCreated[n.name] = old
	}
	for _, n := range []string{mcpNetName(id, 0), relayLinkName(id)} {
		f.netCreated[n] = old
	}
	if _, err := d.SweepOrphanedSandboxes(ctx, time.Minute, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if !gone(f, mcpProxyName(id, 0)) || !gone(f, mcpContainerName(id, 0)) {
		t.Error("the sweep left the orphan's MCP containers")
	}
	for _, n := range []string{mcpNetName(id, 0), relayLinkName(id), mcpNetName(other, 4), relayLinkName(other)} {
		if hasNet(f, n) {
			t.Errorf("the sweep left network %s", n)
		}
	}
}

// TestAttachExecRefuseMCPSandboxRef pins I7: Attach, ExecStream and Exec refuse an MCP container or MCP proxy by
// name, and a container that carries an MCP component label under any other ref, before any exec is made.
func TestAttachExecRefuseMCPSandboxRef(t *testing.T) {
	f, d := newMCPRig(t)
	ctx := context.Background()
	id := testSpec().RunID
	createMCP(t, d, 0)
	f.containers["c0ffee"] = &createdContainer{name: "c0ffee", cfg: &container.Config{Labels: d.labels(id, componentMCPProxy, nil)},
		state: &container.State{Status: "running", Running: true}}
	for _, ref := range []string{mcpContainerName(id, 0), mcpProxyName(id, 0), "/" + mcpContainerName(id, 0), "c0ffee"} {
		if _, err := d.Attach(ctx, ref, runner.AttachOptions{}); !errors.Is(err, errMCPNoShell) {
			t.Errorf("Attach(%s) = %v; want refused", ref, err)
		}
		if _, err := d.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh"}}); !errors.Is(err, errMCPNoShell) {
			t.Errorf("ExecStream(%s) = %v; want refused", ref, err)
		}
		if _, err := d.Exec(ctx, ref, []string{"sh"}); !errors.Is(err, errMCPNoShell) {
			t.Errorf("Exec(%s) = %v; want refused", ref, err)
		}
	}
	if len(f.execLog) != 0 {
		t.Errorf("execs were made: %+v", f.execLog)
	}
	if err := d.refuseMCPRef(ctx, agentContainerName(id)); err != nil {
		t.Errorf("the agent's ref was refused: %v", err)
	}
}

// TestCreateMCPSandboxRefusesWithoutItsContract: a spec Validate refuses, a registered runner and an engine
// older than 26 are refused before anything is created.
func TestCreateMCPSandboxRefusesWithoutItsContract(t *testing.T) {
	ctx := context.Background()
	f, d := newMCPRig(t)
	bad := mcpTestSpec(0, types.CC1)
	bad.ProxyConfig = nil
	if _, err := d.CreateMCPSandbox(ctx, bad); err == nil {
		t.Error("a spec with no proxy config was accepted")
	}
	f.info.ServerVersion = "25.0.3"
	if _, err := d.CreateMCPSandbox(ctx, mcpTestSpec(0, types.CC1)); err == nil || !strings.Contains(err.Error(), "Engine 26") {
		t.Errorf("an Engine 25 create = %v; want refused", err)
	}
	f.info.ServerVersion = "29.4.2"
	d.cfg.RunnerID = "r1"
	if _, err := d.CreateMCPSandbox(ctx, mcpTestSpec(0, types.CC1)); err == nil {
		t.Error("a registered runner created an MCP sandbox")
	}
	if len(f.networks) != 0 || len(f.containers) != 0 {
		t.Errorf("a refused create made objects: networks %v", f.networks)
	}
}

// TestSweepHoldingKillsOrphanMCPSandboxes: a held orphan keeps its stopped agent but none of its MCP sandboxes;
// its relay link stays with the held machine until its teardown.
func TestSweepHoldingKillsOrphanMCPSandboxes(t *testing.T) {
	h := newHoldFixture(t, false)
	h.f.images["wardyn-proxy:dev"] = true
	createMCP(t, h.d, 0)
	if _, heldN := h.sweep(t, holdAll); heldN != 1 {
		t.Fatalf("held %d; want 1", heldN)
	}
	if !gone(h.f, mcpProxyName(h.runID, 0)) || !gone(h.f, mcpContainerName(h.runID, 0)) || hasNet(h.f, mcpNetName(h.runID, 0)) {
		t.Error("a held orphan kept an MCP sandbox")
	}
	if !hasNet(h.f, relayLinkName(h.runID)) || h.removed(h.ref) {
		t.Error("the hold removed the relay link or the agent container")
	}
}
