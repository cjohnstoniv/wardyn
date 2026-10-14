// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// prepareSetupDocker is setup's look at the engine, before the token is spent: it answers as this user,
// and the proxy sidecars' bridge exists (serve would create it, but creating it here surfaces a refusal
// at install time). With relay set it also reads what serve's relayAddress reads, the host-gateway and
// its bridge's addresses, which Docker Desktop does not need.
func prepareSetupDocker(ctx context.Context, network string, relay bool) (setupEngine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return setupEngine{}, err
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return setupEngine{}, fmt.Errorf("docker is not usable as this user: on Linux add yourself to the docker group; on macOS or Windows (WSL2) start Docker Desktop: %w", err)
	}
	eng := setupEngine{desktop: info.Info.OperatingSystem == "Docker Desktop"}
	if relay && !eng.desktop {
		if eng.gateway, eng.bridge, err = dockerHostGateway(ctx, cli); err != nil {
			return setupEngine{}, err
		}
	}
	return eng, ensureInternalNetwork(ctx, cli, network)
}
