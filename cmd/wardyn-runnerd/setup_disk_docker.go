// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vmEngineDiskMiB runs the proxy image's -disk-free on a VM-backed engine.
func vmEngineDiskMiB(ctx context.Context, image string) (int64, bool) {
	if image == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return 0, false
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil || !docker.VMBacked(info.Info, os.Getenv("DOCKER_HOST")) {
		return 0, false
	}
	d, err := docker.New(docker.Config{ProxyImage: image})
	if err != nil {
		return 0, false
	}
	mib, err := d.ProbeDiskFreeMiB(ctx)
	if err != nil {
		slog.DebugContext(ctx, "free disk of the VM-backed engine not measured", "error", err)
		return 0, false
	}
	return mib, true
}

// diskCapEnforced is whether the engine can cap a run's disk; ok is false when it does not answer.
func diskCapEnforced(ctx context.Context) (capped, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return false, false
	}
	defer cli.Close()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, false
	}
	return docker.EphemeralDiskEnforcement(info.Info) != types.StorageEnforcementNone, true
}
