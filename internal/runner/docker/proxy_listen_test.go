// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// deliveredListen is the listen address in the config proxy container name
// received on stdin.
func deliveredListen(t *testing.T, f *fakeDocker, name string) string {
	t.Helper()
	var cfg struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(f.stdinOf(name), &cfg); err != nil {
		t.Fatalf("decode the config %s received: %v", name, err)
	}
	return cfg.Listen
}

// sameConfigExceptListen reports whether two proxy configs are equal once
// the keys the driver writes itself are set aside: the listen address and the
// run's own subnet as allowed_peers (TestProxySandboxPeersOnly_OnCreateAndRevive).
func sameConfigExceptListen(t *testing.T, a, b []byte) bool {
	t.Helper()
	var ma, mb map[string]any
	if err := json.Unmarshal(a, &ma); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	for _, k := range []string{"listen", "allowed_peers"} {
		delete(ma, k)
		delete(mb, k)
	}
	return reflect.DeepEqual(ma, mb)
}

// proxyRunNetIP is the address the run's proxy holds on the per-run network.
func proxyRunNetIP(t *testing.T, d *Driver) string {
	t.Helper()
	id := testSpec().RunID
	res, err := d.cli.ContainerInspect(context.Background(), proxyContainerName(id), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	ep := res.Container.NetworkSettings.Networks[internalNetName(id)]
	if ep == nil || !ep.IPAddress.IsValid() {
		t.Fatalf("proxy has no address on %s", internalNetName(id))
	}
	return ep.IPAddress.String()
}

// createThenReplace runs check after CreateSandbox and again after the
// run's proxy is stopped and replaced.
func createThenReplace(t *testing.T, check func(t *testing.T, f *fakeDocker, d *Driver, sb runner.Sandbox)) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal"})
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Run("create", func(t *testing.T) { check(t, f, d, sb) })
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	t.Run("replace", func(t *testing.T) { check(t, f, d, sb) })
}

// TestProxy_AgentPinsTheAddressAndPortTheProxyServes: on create and after a
// replacement, the agent's wardyn-proxy hosts entry is the proxy's address
// on the per-run network, and the proxy is told to serve ProxyListenPort.
func TestProxy_AgentPinsTheAddressAndPortTheProxyServes(t *testing.T) {
	createThenReplace(t, func(t *testing.T, f *fakeDocker, d *Driver, sb runner.Sandbox) {
		ip := proxyRunNetIP(t, d)
		if got := pinnedProxyIP(f.containers[sb.Ref].host); got.String() != ip {
			t.Errorf("agent pins wardyn-proxy at %s; the proxy holds %s on the run network", got, ip)
		}
		_, port, err := net.SplitHostPort(deliveredListen(t, f, proxyContainerName(testSpec().RunID)))
		if err != nil || port != strconv.Itoa(runner.ProxyListenPort) {
			t.Errorf("proxy listen port = %q (%v); want %d", port, err, runner.ProxyListenPort)
		}
	})
}

// TestProxy_ListensOnlyOnItsRunNetworkAddress: the config a proxy receives
// binds its listener to its own per-run-network address, never to every
// interface or to its address on the control-plane-facing network.
func TestProxy_ListensOnlyOnItsRunNetworkAddress(t *testing.T) {
	createThenReplace(t, func(t *testing.T, f *fakeDocker, d *Driver, sb runner.Sandbox) {
		ip := proxyRunNetIP(t, d)
		listen := deliveredListen(t, f, proxyContainerName(testSpec().RunID))
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			t.Fatalf("listen %q: %v", listen, err)
		}
		switch host {
		case "", "0.0.0.0", "::", fakeJoinedNetIP:
			t.Errorf("proxy listens on %q; want only its run-network address %s", listen, ip)
		}
		if want := net.JoinHostPort(ip, strconv.Itoa(runner.ProxyListenPort)); listen != want {
			t.Errorf("proxy listen = %q; want %q", listen, want)
		}
	})
}

// TestMCPProxyListensOnMCPNetOnly pins I3's listeners: started, an MCP proxy's forward listener binds its MCP
// network address and its relay listener its relay-link address, each keeping its port, its DNS listener binds
// the same MCP network address, and its upstream is the host program in its container. The proxy starts before
// the container.
func TestMCPProxyListensOnMCPNetOnly(t *testing.T) {
	f, d := newMCPRig(t)
	id := testSpec().RunID
	sb := createMCP(t, d, 0)
	if err := d.StartMCPSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("StartMCPSandbox: %v", err)
	}
	proxyIP := pinnedOn(t, f, mcpProxyName(id, 0), mcpNetName(id, 0))
	relayIP := pinnedOn(t, f, mcpProxyName(id, 0), relayLinkName(id))
	ctrIP := pinnedOn(t, f, mcpContainerName(id, 0), mcpNetName(id, 0))
	var cfg struct {
		Listen  string `json:"listen"`
		Inbound struct {
			Listen   string `json:"listen"`
			Upstream string `json:"upstream"`
		} `json:"mcp_relay_inbound"`
	}
	if err := json.Unmarshal(f.stdinOf(mcpProxyName(id, 0)), &cfg); err != nil {
		t.Fatalf("decode the MCP proxy's config: %v", err)
	}
	if want := net.JoinHostPort(proxyIP.String(), "3128"); cfg.Listen != want {
		t.Errorf("forward listen = %q, want %q", cfg.Listen, want)
	}
	if want := net.JoinHostPort(relayIP.String(), "9443"); cfg.Inbound.Listen != want || sb.RelayAddr != want {
		t.Errorf("relay listen = %q, relay address %q; want both %q", cfg.Inbound.Listen, sb.RelayAddr, want)
	}
	if want := net.JoinHostPort(ctrIP.String(), mcpHostPort); cfg.Inbound.Upstream != want {
		t.Errorf("upstream = %q, want %q", cfg.Inbound.Upstream, want)
	}
	if proxyIP == relayIP || proxyIP == ctrIP {
		t.Errorf("addresses proxy %s relay %s container %s must differ", proxyIP, relayIP, ctrIP)
	}
	started := mcpStarted(f)
	if want := []string{mcpProxyName(id, 0), mcpContainerName(id, 0)}; !slices.Equal(started, want) {
		t.Errorf("started %v, want %v", started, want)
	}
}

// TestAgentProxyListenerUnchangedWithRelayLink: on the relay link, the agent's proxy still listens only on its
// run-network address, never on its relay-link one.
func TestAgentProxyListenerUnchangedWithRelayLink(t *testing.T) {
	f, d := newMCPRig(t)
	id := testSpec().RunID
	createMCP(t, d, 0)
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if !onNet(f, proxyContainerName(id), relayLinkName(id)) {
		t.Fatal("the agent's proxy is not on the relay link")
	}
	ip := proxyRunNetIP(t, d)
	if got, want := deliveredListen(t, f, proxyContainerName(id)), net.JoinHostPort(ip, strconv.Itoa(runner.ProxyListenPort)); got != want {
		t.Errorf("agent proxy listen = %q, want %q (its run-network address)", got, want)
	}
}
