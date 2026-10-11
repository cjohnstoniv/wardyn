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
