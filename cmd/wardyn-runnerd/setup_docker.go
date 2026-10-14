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
// at install time).
func prepareSetupDocker(ctx context.Context, network string) (bool, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return false, err
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, fmt.Errorf("docker is not usable as this user: on Linux add yourself to the docker group; on macOS or Windows (WSL2) start Docker Desktop: %w", err)
	}
	return info.Info.OperatingSystem == "Docker Desktop", ensureInternalNetwork(ctx, cli, network)
}
