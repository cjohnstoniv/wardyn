// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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

// TestReplaceProxy_NewProxyExitsAtConfigLoad pins F2: when the REPLACEMENT
// proxy itself exits at config load, the OLD proxy is already gone by the
// time startProxy's exit-watch catches this, so the exited NEW container
// (same deterministic name) must be LEFT IN PLACE rather than removed — it is
// now the only surviving copy of the run's config and MITM CA — and
// ProxyConfig must still read it back, or the run becomes unrevivable.
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
	cfg, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
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
	readBack, err := d.ProxyConfig(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("ProxyConfig after the new proxy died: %v", err)
	}
	if string(readBack) != string(cfg) {
		t.Errorf("config read back = %s; want the config the dying proxy was started with", readBack)
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
