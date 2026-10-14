// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/netip"
	"strings"
	"testing"
)

// relayPlan settles the relay's address and the proxy's hosts target from the engine's facts, judging
// any explicit address only against those facts.
func TestRelayPlan(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	native := engineFacts{os: "Ubuntu 24.04 LTS", name: "laptop", gateway: gw, hostBridge: true, bridge: []netip.Addr{gw}}
	desktop := engineFacts{os: "Docker Desktop", name: "docker-desktop", desktop: true}
	// Docker Desktop on WSL2 beside a distribution that holds a docker0 of its own: still Docker Desktop.
	desktopBesideBridge := engineFacts{os: "Docker Desktop", name: "docker-desktop", desktop: true, gateway: gw, hostBridge: true, bridge: []netip.Addr{gw}}
	vm := engineFacts{os: "Alpine Linux v3.21", name: "lima-rancher-desktop", gateway: gw}
	for _, tc := range []struct {
		name           string
		facts          engineFacts
		bind, target   string // explicit; "" unset
		wantBind, want string // want: the target, or an error substring
	}{
		{"native defaults to its host-gateway", native, "", "", "172.17.0.1", "host-gateway"},
		{"native admits its host-gateway", native, "172.17.0.1", "", "172.17.0.1", "host-gateway"},
		{"native refuses loopback", native, "127.0.0.1", "", "", "reaches only its host-gateway 172.17.0.1"},
		{"native refuses another address", native, "192.168.1.20", "", "", "reaches only its host-gateway"},
		{"native whose bridge does not hold the gateway", engineFacts{gateway: gw, hostBridge: true}, "", "", "", "not an address of the Docker bridge"},
		{"native with an explicit target", native, "", "10.0.0.9", "172.17.0.1", "10.0.0.9"},
		{"Docker Desktop defaults to loopback", desktop, "", "", "127.0.0.1", "host-gateway"},
		{"Docker Desktop admits loopback", desktop, "127.0.0.1", "", "127.0.0.1", "host-gateway"},
		{"Docker Desktop refuses another address", desktop, "192.168.1.20", "", "", "reaches only 127.0.0.1"},
		{"Docker Desktop refuses every interface", desktop, "0.0.0.0", "", "", "one address of this host"},
		{"Docker Desktop beside a host bridge", desktopBesideBridge, "", "", "127.0.0.1", "host-gateway"},
		{"VM-backed engine", vm, "", "", "", `OperatingSystem "Alpine Linux v3.21", Name "lima-rancher-desktop"`},
		{"VM-backed engine with an explicit loopback", vm, "127.0.0.1", "", "", "not supported yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var explicit netip.Addr
			if tc.bind != "" {
				explicit = netip.MustParseAddr(tc.bind)
			}
			bind, target, err := relayPlan(tc.facts, explicit, tc.target)
			if tc.wantBind == "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("relayPlan = %s %s %v, want an error naming %q", bind, target, err, tc.want)
				}
				return
			}
			if err != nil || bind.String() != tc.wantBind || target != tc.want {
				t.Fatalf("relayPlan = %s %q %v, want %s %q", bind, target, err, tc.wantBind, tc.want)
			}
		})
	}
}

func TestCheckRelayHostTarget(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "host-gateway": true, "192.168.5.2": true, "fd00::2": true,
		"127.0.0.1": false, "::1": false, "0.0.0.0": false, "224.0.0.1": false, "host.docker.internal": false} {
		if err := checkRelayHostTarget(v); (err == nil) != ok {
			t.Errorf("checkRelayHostTarget(%q) = %v, want ok=%v", v, err, ok)
		}
	}
}
