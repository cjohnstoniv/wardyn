// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type fakeNetworkInspector struct {
	res client.NetworkInspectResult
	err error
}

func (f fakeNetworkInspector) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return f.res, f.err
}

// TestControlPlaneNetworkSubnets (#1198): the docker-tagged half of the
// boot-time Bedrock/proxy-subnet check — a network with an IPAM subnet
// reports it, a daemon/lookup error or a network with no configured subnet
// reports unknown (ok=false) rather than guessing.
func TestControlPlaneNetworkSubnets(t *testing.T) {
	subnet := netip.MustParsePrefix("172.30.0.0/16")

	for name, c := range map[string]struct {
		fake        fakeNetworkInspector
		wantOK      bool
		wantSubnets []netip.Prefix
	}{
		"IPAM subnet present": {
			fake: fakeNetworkInspector{res: client.NetworkInspectResult{Network: network.Inspect{
				Network: network.Network{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}}},
			}}},
			wantOK:      true,
			wantSubnets: []netip.Prefix{subnet},
		},
		"daemon error reports unknown": {
			fake:   fakeNetworkInspector{err: errors.New("no such network")},
			wantOK: false,
		},
		"no IPAM config reports unknown": {
			fake:   fakeNetworkInspector{res: client.NetworkInspectResult{}},
			wantOK: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prev := newNetworkInspector
			newNetworkInspector = func() (networkInspector, error) { return c.fake, nil }
			t.Cleanup(func() { newNetworkInspector = prev })

			subnets, ok := controlPlaneNetworkSubnets(context.Background(), "wardyn-internal")
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (subnets = %v)", ok, c.wantOK, subnets)
			}
			if ok && (len(subnets) != len(c.wantSubnets) || subnets[0] != c.wantSubnets[0]) {
				t.Errorf("subnets = %v, want %v", subnets, c.wantSubnets)
			}
		})
	}
}

// TestControlPlaneNetworkSubnets_ClientConstructFailure: a docker client that
// cannot even be constructed (no daemon reachable at boot) reports unknown,
// never guesses.
func TestControlPlaneNetworkSubnets_ClientConstructFailure(t *testing.T) {
	prev := newNetworkInspector
	newNetworkInspector = func() (networkInspector, error) { return nil, errors.New("no daemon") }
	t.Cleanup(func() { newNetworkInspector = prev })

	if _, ok := controlPlaneNetworkSubnets(context.Background(), "wardyn-internal"); ok {
		t.Error("expected ok=false when the docker client cannot be constructed")
	}
}
