// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"context"
	"net/netip"
)

// controlPlaneNetworkSubnets: a tagless wardynd carries no Docker client (the
// parity rule — see envbuild_nodocker.go for the same split), so it can never
// report the control-plane network's subnet here. In practice this path is
// unreachable with a real docker runner (the "docker" substrate itself
// requires -tags docker to register at all, so buildRunnerFromFlags would
// already have refused boot with runnerTarget=="docker" on a tagless build);
// it only fires under the -runner-target test-harness override.
func controlPlaneNetworkSubnets(ctx context.Context, networkName string) ([]netip.Prefix, bool) {
	return nil, false
}
