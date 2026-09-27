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

// TestReplaceProxy_ALostRunsProxyComesBackAtItsAddress is proxy-only revive on
// Docker (long-holds design rev 4 §4.1): a lost run's stopped proxy gives its
// config back, the old container is removed, and a new one starts from the
// rewritten config on the current proxy image, at the address the agent's
// hosts entry pins, re-joined to the control-plane network. The agent is not
// touched.
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
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	old := f.containers[proxyContainerName(runID)]

	cfg, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig of a stopped proxy: %v", err)
	}
	if !strings.Contains(string(cfg), `"run_token":"tok"`) {
		t.Fatalf("config read back = %s; want the rendered config with its run token", cfg)
	}
	d.cfg.ProxyImage = "wardyn-proxy:next"
	fresh := strings.Replace(string(cfg), `"run_token":"tok"`, `"run_token":"fresh"`, 1)
	if err := d.ReplaceProxy(ctx, sb.Ref, []byte(fresh)); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}

	if !old.removed {
		t.Error("the retiring proxy was not removed")
	}
	p := f.containers[proxyContainerName(runID)]
	if p == old || p == nil || p.state == nil || !p.state.Running {
		t.Fatalf("new proxy = %+v; want a new, running container", p)
	}
	if !slices.Contains(p.cfg.Env, proxyConfigEnv+"="+fresh) {
		t.Errorf("new proxy env = %v; want the rewritten config", p.cfg.Env)
	}
	if p.cfg.Image != "wardyn-proxy:next" || p.cfg.Labels[labelRun] != runID.String() {
		t.Errorf("new proxy image %q labels %v; want the current image and the old labels", p.cfg.Image, p.cfg.Labels)
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
// gone (the run then has no egress, never the old token's proxy back), and a
// sandbox whose proxy is gone has no config to read back.
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
	cfg, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
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
	if _, err := d.ProxyConfig(ctx, sb.Ref); err == nil {
		t.Error("ProxyConfig of a removed proxy succeeded")
	}
}

// TestReplaceProxy_ARefusedPinLeavesTheOldProxyRestored (#1133): Docker Engine
// 28 refuses the new proxy's pinned address on a per-run network whose create
// named no subnet (one made before per-run networks named theirs). The replace
// fails as ErrProxyReplaceFailed, and the old proxy is back under its name,
// stopped, with its own config, so a later revive can still read it.
func TestReplaceProxy_ARefusedPinLeavesTheOldProxyRestored(t *testing.T) {
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
	cfg, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
	old := f.containers[proxyContainerName(runID)]
	f.networks[internalNetName(runID)] = client.NetworkCreateOptions{Driver: "bridge", Internal: true}

	err = d.ReplaceProxy(ctx, sb.Ref, []byte(strings.Replace(string(cfg), `"run_token":"tok"`, `"run_token":"fresh"`, 1)))
	if !errors.Is(err, runner.ErrProxyReplaceFailed) || !strings.Contains(err.Error(), "user configured subnets") {
		t.Fatalf("ReplaceProxy with the pin refused = %v; want ErrProxyReplaceFailed carrying the daemon's refusal", err)
	}
	p := f.containers[proxyContainerName(runID)]
	if p == nil || p == old || p.removed || p.state.Running {
		t.Fatalf("proxy after the refused replace = %+v; want the old one restored, not running", p)
	}
	if p.cfg != old.cfg || p.host != old.host {
		t.Error("the restored proxy does not carry the old proxy's config and hardening")
	}
	if got, err := d.ProxyConfig(ctx, sb.Ref); err != nil || string(got) != string(cfg) {
		t.Errorf("ProxyConfig after the refused replace = %q, %v; want the old config back", got, err)
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

// TestStartSandbox_OnlyBehindARunningProxy is revive after a reboot on Docker
// (long-holds design rev 4 §4 row 3): a kept agent is started again only once
// its proxy runs. A stopped proxy has given its address back, and an agent
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
		t.Fatalf("StartSandbox behind a stopped proxy = %v, agent running %v; want a refusal and the agent left stopped", err, agent.state.Running)
	}
	cfg, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if err := d.StartSandbox(ctx, sb.Ref); err != nil || !agent.state.Running || agent.removed {
		t.Fatalf("StartSandbox behind the new proxy = %v, agent %+v; want the kept agent running", err, agent)
	}
	if err := d.StartSandbox(ctx, "wardyn-agent-not-a-run"); err == nil {
		t.Error("StartSandbox of an unresolvable ref succeeded")
	}
}
