// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestProxyExtraHostsMapOnlyTheProxy: on a runner, the run's control-plane
// hostname resolves to host-gateway (the runner's relay) in the proxy's hosts
// and nowhere in the agent's, whose only route is the proxy. An IP-literal URL
// cannot be remapped and refuses the create before any container exists.
func TestProxyExtraHostsMapOnlyTheProxy(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	var relayed []string
	d.cfg.ControlPlaneRelay = func(u *url.URL) error { relayed = append(relayed, u.Host); return nil }
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	const entry = "wardynd.internal:host-gateway"
	proxy, agent := f.containers[proxyContainerName(spec.RunID)], f.containers[agentContainerName(spec.RunID)]
	if proxy == nil || agent == nil {
		t.Fatal("the sandbox was not created")
	}
	if !slices.Contains(proxy.host.ExtraHosts, entry) {
		t.Fatalf("proxy hosts %v lack %s", proxy.host.ExtraHosts, entry)
	}
	for _, h := range agent.host.ExtraHosts {
		if strings.HasPrefix(h, "wardynd.internal:") || strings.HasSuffix(h, ":host-gateway") {
			t.Fatalf("the agent's hosts map %q: only the proxy reaches the relay", h)
		}
	}
	if !slices.Equal(relayed, []string{"wardynd.internal:8443"}) {
		t.Fatalf("relay asked to listen for %v", relayed)
	}

	f2 := newFakeDocker()
	f2.images["busybox:latest"] = true
	d2 := newTestDriver(f2)
	d2.cfg.ControlPlaneRelay = func(*url.URL) error { return nil }
	spec.ProxyConfig.ControlPlaneURL = "https://10.0.0.5:8443"
	if _, err := d2.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "IP address") {
		t.Fatalf("an IP-literal control-plane URL was relayed: %v", err)
	}
	if c := f2.containers[agentContainerName(spec.RunID)]; c != nil && !c.removed {
		t.Fatal("an agent was left behind by a refused create")
	}
}

// TestRelayHostsEntryTarget: the hosts entry maps the control-plane name to host-gateway
// by default and to the configured literal IP when ControlPlaneHostTarget is set.
func TestRelayHostsEntryTarget(t *testing.T) {
	cfg := []byte(`{"control_plane_url":"https://wardynd.internal:8443"}`)
	for _, tc := range []struct{ target, want string }{
		{"", "wardynd.internal:host-gateway"},
		{"host-gateway", "wardynd.internal:host-gateway"},
		{"192.168.5.2", "wardynd.internal:192.168.5.2"},
	} {
		d := newTestDriver(newFakeDocker())
		d.cfg.ControlPlaneRelay = func(*url.URL) error { return nil }
		d.cfg.ControlPlaneHostTarget = tc.target
		got, err := d.relayHostsEntry(cfg)
		if err != nil || got != tc.want {
			t.Fatalf("target %q: entry %q, %v; want %q", tc.target, got, err, tc.want)
		}
	}
}

// TestProxyExtraHostsCarryTheConfiguredTarget: a created proxy gets the configured target,
// the host-mode host.docker.internal alias is untouched, and the agent still gets neither.
func TestProxyExtraHostsCarryTheConfiguredTarget(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	d.cfg.ControlPlaneRelay = func(*url.URL) error { return nil }
	d.cfg.ControlPlaneHostTarget = "192.168.5.2"
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	proxy, agent := f.containers[proxyContainerName(spec.RunID)], f.containers[agentContainerName(spec.RunID)]
	if !slices.Contains(proxy.host.ExtraHosts, "wardynd.internal:192.168.5.2") ||
		!slices.Contains(proxy.host.ExtraHosts, "host.docker.internal:host-gateway") {
		t.Fatalf("proxy hosts %v", proxy.host.ExtraHosts)
	}
	for _, h := range agent.host.ExtraHosts {
		if strings.HasPrefix(h, "wardynd.internal:") || strings.Contains(h, "192.168.5.2") {
			t.Fatalf("the agent's hosts map %q", h)
		}
	}
}

func TestConfigValidatesControlPlaneHostTarget(t *testing.T) {
	for _, ok := range []string{"", "host-gateway", "192.168.5.2", "10.0.2.2", "172.16.0.1"} {
		if err := (&Config{ControlPlaneHostTarget: ok}).validate(); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "localhost", "host.docker.internal", "192.168.5", "1.2.3.4:80", " "} {
		if err := (&Config{ControlPlaneHostTarget: bad}).validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := New(Config{ControlPlaneHostTarget: "127.0.0.1"}); err == nil {
		t.Error("New accepted a loopback target")
	}
}
