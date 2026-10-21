// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
)

// readySetupEngine is setup's first look at the engine, before the token is spent: serve's own
// readyEngine, run as this user with the network setup writes for serve.
func readySetupEngine(ctx context.Context, network string, want relayPair) (dockerEngine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return dockerEngine{}, err
	}
	defer cli.Close()
	return readyEngine(ctx, cli, setupEngineUnreachable, network, want)
}

const setupEngineUnreachable = "docker is not usable as this user: on Linux add yourself to the docker group; on macOS or Windows (WSL2) start " +
	"Docker Desktop, Rancher Desktop or Colima, and set DOCKER_HOST to its socket if it is not the default"

// inspectSetupEngine is readySetupEngine for doctor: the same look at the engine, without creating the
// proxy bridge.
func inspectSetupEngine(ctx context.Context, want relayPair) (dockerEngine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return dockerEngine{}, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(ctx, engineCheckBound)
	defer cancel()
	return admittedEngine(ctx, cli, setupEngineUnreachable, want)
}

// resolveProxyImage is where the proxy image can be had from without pulling it: this engine ("local"), else
// the registry, which the engine asks for the manifest.
func resolveProxyImage(ctx context.Context, ref string) (string, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return "", err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(ctx, engineCheckBound)
	defer cancel()
	switch _, err := cli.ImageInspect(ctx, ref); {
	case err == nil:
		return "local", nil
	case !errdefs.IsNotFound(err):
		return "", err
	}
	if _, err := cli.DistributionInspect(ctx, ref, client.DistributionInspectOptions{}); err != nil {
		return "", err
	}
	return "registry", nil
}

// probeSetupRelay is serve's chooseRelay (pull the proxy image, then find the relay pair a probe
// container reaches) with the image setup writes for serve. It is a separate step because on a first
// registration the image is not known until the organisation has answered.
func probeSetupRelay(ctx context.Context, image, network string, eng dockerEngine, want relayPair) (relayPair, error) {
	d, err := docker.New(docker.Config{ProxyImage: image, InternalNetwork: network})
	if err != nil {
		return relayPair{}, err
	}
	// Getting the image is told apart from the probe itself: an image setup cannot get may be one the
	// organisation's answer replaces, but a probe that ran and failed never is.
	if err := ensureProxyImage(ctx, d, image); err != nil {
		return relayPair{}, fmt.Errorf("%w: %s (a registry that needs a login: docker pull it as this user first): %v", errProxyImageUnavailable, image, err)
	}
	return chooseRelay(ctx, d, image, eng, want)
}

func ensureProxyImage(ctx context.Context, d *docker.Driver, image string) error {
	ctx, cancel := context.WithTimeout(ctx, relayCheckBound)
	defer cancel()
	present, err := d.ImagePresent(ctx, image)
	if err == nil && !present {
		slog.InfoContext(ctx, "pulling proxy image "+image)
		err = d.EnsureProxyImage(ctx)
	}
	return err
}
