// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
)

// prepareSetupDocker is setup's look at the engine, before the token is spent: serve's own readyEngine
// and chooseRelay (pull the proxy image, then find the relay pair a probe container reaches), run as this
// user with the image and network setup writes for serve.
func prepareSetupDocker(ctx context.Context, image, network string, want relayPair) (dockerEngine, relayPair, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return dockerEngine{}, relayPair{}, err
	}
	defer cli.Close()
	eng, err := readyEngine(ctx, cli, "docker is not usable as this user: on Linux add yourself to the docker group; on macOS or Windows (WSL2) start "+
		"Docker Desktop, Rancher Desktop or Colima, and set DOCKER_HOST to its socket if it is not the default", network, want)
	if err != nil {
		return dockerEngine{}, relayPair{}, err
	}
	d, err := docker.New(docker.Config{ProxyImage: image, InternalNetwork: network})
	if err != nil {
		return dockerEngine{}, relayPair{}, err
	}
	pair, err := chooseRelay(ctx, d, image, eng, want)
	return eng, pair, err
}
