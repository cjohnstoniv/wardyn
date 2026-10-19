// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/netip"
	"slices"
	"testing"
)

func TestDefaultGatewayReadsTheRouteTable(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0000590A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0100590A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	if gw, ok := defaultGateway(table); !ok || gw != netip.MustParseAddr("10.89.0.1") {
		t.Fatalf("defaultGateway = %v, %v; want 10.89.0.1", gw, ok)
	}
	if _, ok := defaultGateway("Iface\tDestination\tGateway \tFlags\neth0\t0000590A\t00000000\t0001\n"); ok {
		t.Fatal("a table with no default route named a gateway")
	}
}

func TestLocalTrustedPeersConfigured(t *testing.T) {
	got, err := localTrustedPeers(" 192.168.65.1, 10.0.0.0/8 ,fd00::1")
	want := []netip.Prefix{netip.MustParsePrefix("192.168.65.1/32"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::1/128")}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("localTrustedPeers = %v, %v; want %v", got, err, want)
	}
	if _, err := localTrustedPeers("not-an-address"); err == nil {
		t.Fatal("an unparsable entry was accepted")
	}
}
