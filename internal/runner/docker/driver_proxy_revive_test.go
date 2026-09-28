// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// renderedTestConfig is testSpec's proxy config as the control plane stores
// it, with its run token swapped for tok.
func renderedTestConfig(t *testing.T, tok string) []byte {
	t.Helper()
	pc := testSpec().ProxyConfig
	pc.RunToken = tok
	cfg, err := runner.BuildProxyConfig(testSpec().RunID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestReplaceProxy_ALostRunsProxyComesBackAtItsAddress is proxy-only revive on
// Docker (long-holds design rev 4 §4.1; #1176): a lost run's proxy is stopped
// and removed, and a new one starts from the config the control plane hands
// in, on the current proxy image, at the address the agent's hosts entry
// pins, re-joined to the control-plane network. The config reaches it on
// stdin, attached before the start; its env carries no config. The agent is
// not touched.
func TestReplaceProxy_ALostRunsProxyComesBackAtItsAddress(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal"})
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	old := f.containers[proxyContainerName(runID)]
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if !old.removed {
		t.Fatal("a lost run's proxy was kept; nothing a revive needs is in it, so it must be removed")
	}

	d.cfg.ProxyImage = "wardyn-proxy:next"
	fresh := renderedTestConfig(t, "fresh")
	f.attached = nil
	if err := d.ReplaceProxy(ctx, sb.Ref, fresh); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	p := f.containers[proxyContainerName(runID)]
	if p == old || p == nil || p.state == nil || !p.state.Running {
		t.Fatalf("new proxy = %+v; want a new, running container", p)
	}
	if got := f.stdinOf(proxyContainerName(runID)); string(got) != string(fresh) {
		t.Errorf("new proxy stdin = %s; want the config handed in", got)
	}
	if len(f.attached) != 1 || f.attached[0].started {
		t.Errorf("attaches = %+v; want one, made before the proxy started", f.attached)
	}
	for _, e := range p.cfg.Env {
		if strings.Contains(e, "fresh") {
			t.Errorf("new proxy env carries the config: %s", e)
		}
	}
	if p.host.RestartPolicy.Name != "no" {
		t.Errorf("new proxy restart policy = %q; want none", p.host.RestartPolicy.Name)
	}
	if p.cfg.Image != "wardyn-proxy:next" || p.cfg.Labels[labelRun] != runID.String() || p.cfg.Labels[labelComponent] != componentProxy {
		t.Errorf("new proxy image %q labels %v; want the current image and proxy labels for the run", p.cfg.Image, p.cfg.Labels)
	}
	ep := p.net.EndpointsConfig[internalNetName(runID)]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != "10.88.0.2" {
		t.Errorf("new proxy endpoint = %+v; want it pinned to the agent's wardyn-proxy address 10.88.0.2", ep)
	}
	if !slices.Contains(p.connectedTo, "wardyn-internal") {
		t.Errorf("new proxy networks = %v; want it re-joined to wardyn-internal", p.connectedTo)
	}
	if agent := f.containers[sb.Ref]; agent.removed || !agent.state.Running {
		t.Errorf("agent after ReplaceProxy = %+v; want it untouched", agent)
	}
}

// TestReplaceProxy_FailsClosed: a proxy image that cannot be pulled fails
// before the old proxy is touched, without ErrProxyReplaceFailed. A new proxy
// that cannot start reports ErrProxyReplaceFailed with the old one already
// gone (the run then has no egress, never the old token's proxy back).
func TestReplaceProxy_FailsClosed(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	cfg := renderedTestConfig(t, "fresh")
	old := f.containers[proxyContainerName(runID)]
	present := f.images
	f.images, f.failImagePull = map[string]bool{}, true
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err == nil || errors.Is(err, runner.ErrProxyReplaceFailed) || old.removed {
		t.Fatalf("ReplaceProxy with an unpullable image = %v, old proxy removed %v; want a plain error and the old proxy kept", err, old.removed)
	}
	f.images, f.failImagePull = present, false
	f.failCreateContainer = "wardyn-proxy-"
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); !errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy with a failing create = %v, want ErrProxyReplaceFailed", err)
	}
	if !old.removed {
		t.Error("the retiring proxy must be gone before the new one is created")
	}
}

// TestReplaceProxy_ARefusedPinFailsAsReplaceFailed (#1133): Docker Engine 28
// refuses the new proxy's pinned address on a per-run network whose create
// named no subnet (one made before per-run networks named theirs). The replace
// fails as ErrProxyReplaceFailed carrying the daemon's refusal, and nothing is
// put back: the config is the control plane's (#1176), so a later revive
// rebuilds from it.
func TestReplaceProxy_ARefusedPinFailsAsReplaceFailed(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	f.networks[internalNetName(runID)] = client.NetworkCreateOptions{Driver: "bridge", Internal: true}

	err = d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh"))
	if !errors.Is(err, runner.ErrProxyReplaceFailed) || !strings.Contains(err.Error(), "user configured subnets") {
		t.Fatalf("ReplaceProxy with the pin refused = %v; want ErrProxyReplaceFailed carrying the daemon's refusal", err)
	}
	if p := f.containers[proxyContainerName(runID)]; p != nil && !p.removed {
		t.Errorf("proxy after the refused replace = %+v; want none", p)
	}
}

// TestCreateSandbox_TheRunNetworkNamesItsSubnet (#1133): the per-run network
// is created asking for a subnet, so the proxy's address can be pinned on it
// on Docker Engine 28. The subnet is the one the daemon picked; one that
// another network took in between is picked again, never shared.
func TestCreateSandbox_TheRunNetworkNamesItsSubnet(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	var rival netip.Prefix
	f.onNetworkCreate = func(name string, opts client.NetworkCreateOptions) {
		if opts.IPAM == nil || rival.IsValid() {
			return
		}
		rival = opts.IPAM.Config[0].Subnet
		f.mu.Lock()
		f.networks["rival"] = client.NetworkCreateOptions{IPAM: &network.IPAM{Config: []network.IPAMConfig{{Subnet: rival}}}}
		f.subnets["rival"] = rival
		f.mu.Unlock()
	}
	if _, err := newTestDriver(f).CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	opts := f.networks[internalNetName(testSpec().RunID)]
	if opts.IPAM == nil || len(opts.IPAM.Config) != 1 || !opts.IPAM.Config[0].Subnet.IsValid() || !opts.Internal {
		t.Fatalf("per-run network create = %+v; want it internal and asking for one subnet", opts)
	}
	if got := opts.IPAM.Config[0].Subnet; got == rival {
		t.Errorf("per-run network subnet %s; want a fresh pick, not the %s another network took", got, rival)
	}
}

// TestReplaceProxy_NewProxyExitsAtConfigLoad: when the REPLACEMENT proxy
// itself exits at config load, the OLD proxy is already gone by the time
// startProxy's exit-watch catches this; the error names the config-load cause
// and the exited NEW container (same deterministic name) is LEFT IN PLACE, so
// its logs still say why.
func TestReplaceProxy_NewProxyExitsAtConfigLoad(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	runID := testSpec().RunID
	cfg := renderedTestConfig(t, "fresh")
	old := f.containers[proxyContainerName(runID)]

	// A fake-backed driver defaults proxySettle to 0; exercise the real
	// settle window on the REPLACEMENT proxy's start below (set after the
	// initial CreateSandbox above, so its own healthy-proxy start stays fast).
	d.proxySettle = proxyStartSettle

	// The replacement proxy reuses the OLD proxy's deterministic name, and
	// ReplaceProxy inspects the OLD proxy once (for its labels) before
	// removing it — that inspect must not consume the NEW container's
	// exitAfterInspects budget, so onCreate resets the counter the instant
	// the NEW container is actually created.
	f.exitAfterInspects = map[string]int{proxyContainerName(runID): 2}
	f.logs = map[string][]byte{proxyContainerName(runID): muxFrame(1, `unknown field "y"`)}
	f.onCreate = func(name string) {
		if name == proxyContainerName(runID) {
			f.mu.Lock()
			delete(f.inspectCounts, name)
			f.mu.Unlock()
		}
	}

	err = d.ReplaceProxy(ctx, sb.Ref, cfg)
	if !errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy with a dying new proxy = %v, want ErrProxyReplaceFailed", err)
	}
	if !strings.Contains(err.Error(), "proxy exited at config load (exit 1)") {
		t.Errorf("error = %v; want the named config-load cause", err)
	}
	if !old.removed {
		t.Error("the retiring proxy must already be gone before the new one is attempted")
	}
	if p := f.containers[proxyContainerName(runID)]; p == nil || p.removed {
		t.Fatalf("new (exited) proxy = %+v; want it left in place, not removed", p)
	}
}

// TestStartSandbox_OnlyBehindARunningProxy is revive after a reboot on Docker
// (long-holds design rev 4 §4 row 3): a kept agent is started again only once
// its proxy runs. A removed proxy has given its address back, and an agent
// started first could take the address its own hosts entry pins.
func TestStartSandbox_OnlyBehindARunningProxy(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal"})
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.EndSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	agent := f.containers[sb.Ref]
	if err := d.StartSandbox(ctx, sb.Ref); err == nil || agent.state.Running {
		t.Fatalf("StartSandbox with no proxy = %v, agent running %v; want a refusal and the agent left stopped", err, agent.state.Running)
	}
	if p := f.containers[proxyContainerName(testSpec().RunID)]; p == nil || !p.removed {
		t.Fatalf("proxy after EndSandbox = %+v; want it removed", p)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if err := d.StartSandbox(ctx, sb.Ref); err != nil || !agent.state.Running || agent.removed {
		t.Fatalf("StartSandbox behind the new proxy = %v, agent %+v; want the kept agent running", err, agent)
	}
	if err := d.StartSandbox(ctx, "wardyn-agent-not-a-run"); err == nil {
		t.Error("StartSandbox of an unresolvable ref succeeded")
	}
}
