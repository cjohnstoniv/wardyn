// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"net/netip"

	"github.com/moby/moby/client"
)

// networkInspector is the narrow docker-client slice controlPlaneNetworkSubnets
// needs, kept separate from internal/runner/docker's own dockerAPI (a
// different package with a much wider need) so a fake can back
// TestControlPlaneNetworkSubnets without a live daemon.
type networkInspector interface {
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
}

// the real client must implement it.
var _ networkInspector = (*client.Client)(nil)

// newNetworkInspector is a seam tests replace; production dials the daemon
// FromEnv exactly like internal/runner/docker.New does.
var newNetworkInspector = func() (networkInspector, error) {
	return client.New(client.FromEnv)
}

// controlPlaneNetworkSubnets asks the Docker daemon for the IPAM subnet(s) of
// the named bridge network — the control-plane network every wardyn-proxy
// sidecar joins after its per-run network is created (driver_network.go
// NetworkConnect). Compiled only under -tags docker (see
// bedrock_subnet_nodocker.go for the tagless stub): a docker daemon this
// process cannot reach, or a network that does not exist yet, reports
// unknown (ok=false) rather than guessing at a subnet.
func controlPlaneNetworkSubnets(ctx context.Context, networkName string) ([]netip.Prefix, bool) {
	cli, err := newNetworkInspector()
	if err != nil {
		return nil, false
	}
	if closer, ok := cli.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	res, err := cli.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
	if err != nil {
		return nil, false
	}
	var subnets []netip.Prefix
	for _, cfg := range res.Network.IPAM.Config {
		if cfg.Subnet.IsValid() {
			subnets = append(subnets, cfg.Subnet)
		}
	}
	return subnets, len(subnets) > 0
}
