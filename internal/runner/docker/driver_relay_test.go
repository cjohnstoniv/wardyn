// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
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
	d.cfg.ControlPlaneRelay = func(u *url.URL) (string, error) { relayed = append(relayed, u.Host); return "host-gateway", nil }
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	const entry = "wardynd.internal:host-gateway"
	if want := []string{"host.docker.internal:host-gateway", entry}; !slices.Equal(f.containers[proxyContainerName(spec.RunID)].host.ExtraHosts, want) {
		t.Fatalf("proxy hosts %v, want %v", f.containers[proxyContainerName(spec.RunID)].host.ExtraHosts, want)
	}
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
	d2.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
	spec.ProxyConfig.ControlPlaneURL = "https://10.0.0.5:8443"
	if _, err := d2.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "IP address") {
		t.Fatalf("an IP-literal control-plane URL was relayed: %v", err)
	}
	if c := f2.containers[agentContainerName(spec.RunID)]; c != nil && !c.removed {
		t.Fatal("an agent was left behind by a refused create")
	}
}

// TestProxyExtraHostsFollowTheRelayTarget: on a runner both the control-plane name and
// host.docker.internal map to the target the relay callback returns, so the proxy's
// control-plane clamp covers the one address either resolves to; the agent gets neither.
func TestProxyExtraHostsFollowTheRelayTarget(t *testing.T) {
	for _, target := range []string{"host-gateway", "192.168.5.2"} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newTestDriver(f)
		d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return target, nil }
		spec := testSpec()
		spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
		if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		proxy, agent := f.containers[proxyContainerName(spec.RunID)], f.containers[agentContainerName(spec.RunID)]
		want := []string{"host.docker.internal:" + target, "wardynd.internal:" + target}
		if !slices.Equal(proxy.host.ExtraHosts, want) {
			t.Fatalf("target %s: proxy hosts %v, want %v", target, proxy.host.ExtraHosts, want)
		}
		for _, h := range agent.host.ExtraHosts {
			if strings.HasPrefix(h, "wardynd.internal:") || strings.HasPrefix(h, "host.docker.internal:") || strings.Contains(h, target) {
				t.Fatalf("target %s: the agent's hosts map %q", target, h)
			}
		}
	}
}

// TestProxyExtraHostsWithoutARelayAreUnchanged pins wardynd: no relay callback leaves the
// proxy's hosts exactly the host-mode alias, as before.
func TestProxyExtraHostsWithoutARelayAreUnchanged(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := f.containers[proxyContainerName(spec.RunID)].host.ExtraHosts; !slices.Equal(got, []string{"host.docker.internal:host-gateway"}) {
		t.Fatalf("proxy hosts %v", got)
	}
}

// TestRelayHostsEntryRefusesAnUnusableTarget: the callback's target is validated, so a
// loopback, unspecified, multicast or non-IP answer refuses the create.
func TestRelayHostsEntryRefusesAnUnusableTarget(t *testing.T) {
	cfg := []byte(`{"control_plane_url":"https://wardynd.internal:8443"}`)
	for _, ok := range []string{"host-gateway", "192.168.5.2", "10.0.2.2", "172.16.0.1"} {
		d := newTestDriver(newFakeDocker())
		d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return ok, nil }
		name, target, err := d.relayHostsEntry(cfg)
		if err != nil || name != "wardynd.internal" || target != ok {
			t.Errorf("%q: %q %q %v", ok, name, target, err)
		}
	}
	for _, bad := range []string{"", "127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "::ffff:0.0.0.0", "::ffff:127.0.0.1", "::ffff:224.0.0.1", "fe80::1%eth0", "localhost", "192.168.5", "1.2.3.4:80"} {
		d := newTestDriver(newFakeDocker())
		d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return bad, nil }
		if _, _, err := d.relayHostsEntry(cfg); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	d := newTestDriver(newFakeDocker())
	d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "", errors.New("no relay") }
	if _, _, err := d.relayHostsEntry(cfg); err == nil || !strings.Contains(err.Error(), "no relay") {
		t.Errorf("callback error lost: %v", err)
	}
}
