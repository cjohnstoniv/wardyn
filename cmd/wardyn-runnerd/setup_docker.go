// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
)

// prepareSetupDocker is setup's look at the engine, before the token is spent: it answers as this user,
// with the facts serve's relayPlan judges (dockerEngineFacts), and the proxy sidecars' bridge exists
// (serve would create it, but creating it here surfaces a refusal at install time).
func prepareSetupDocker(ctx context.Context, network string) (engineFacts, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return engineFacts{}, err
	}
	defer cli.Close()
	f, err := dockerEngineFacts(ctx, cli, "docker is not usable as this user: on Linux add yourself to the docker group; on macOS or Windows (WSL2) start Docker Desktop")
	if err != nil {
		return engineFacts{}, err
	}
	return f, ensureInternalNetwork(ctx, cli, network)
}

// setupRelayProbe is serve's reachability check (checkRelayReach) run from setup, before the token is
// spent, with a driver built for it alone. The proxy image is pulled when it is missing.
func setupRelayProbe(ctx context.Context, network, proxyImage string, at relayChoice) error {
	d, err := docker.New(docker.Config{ProxyImage: proxyImage, InternalNetwork: network})
	if err != nil {
		return err
	}
	return checkRelayReach(ctx, d, at)
}
