// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Measure implements runner.Measurer: the CPUs and memory Docker reports, and the free space of
// Docker's data root when it is a path on this host (not on a VM-backed engine).
func (d *Driver) Measure(ctx context.Context) (types.RunnerAmounts, error) {
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return types.RunnerAmounts{}, fmt.Errorf("docker: info: %w", err)
	}
	return MeasureInfo(res.Info, os.Getenv("DOCKER_HOST"), FreeDiskMiB), nil
}

// VMBackedEngine asks the engine whether it is VM-backed (see VMBacked).
func (d *Driver) VMBackedEngine(ctx context.Context) (bool, error) {
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, fmt.Errorf("docker: info: %w", err)
	}
	return VMBacked(res.Info, os.Getenv("DOCKER_HOST")), nil
}

// Close releases the driver's engine client.
func (d *Driver) Close() error {
	if c, ok := d.cli.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// vmSocketDirs are the directories a VM-backed engine's socket lives under on this host: Docker
// Desktop, Colima, Rancher Desktop and Lima.
var vmSocketDirs = []string{"/.docker/run/", "/.docker/desktop/", "/.colima/", "/.rd/", "/.lima/"}

// VMBacked reports whether the engine runs in a VM (or on another machine), so that its data root is
// not a path on this host: Docker Desktop, Colima, Rancher Desktop, Lima, or any DOCKER_HOST that is
// not a unix socket. It reads what the engine calls itself and the socket path it is reached by.
func VMBacked(info system.Info, dockerHost string) bool {
	if !localSocket(dockerHost) || info.OperatingSystem == "Docker Desktop" {
		return true
	}
	name := strings.ToLower(info.Name)
	if strings.HasPrefix(name, "colima") || strings.HasPrefix(name, "lima-") || strings.Contains(name, "rancher-desktop") {
		return true
	}
	for _, dir := range vmSocketDirs {
		if strings.Contains(dockerHost, dir) {
			return true
		}
	}
	return false
}

// localSocket reports whether DOCKER_HOST names a daemon on this host (unset is the local default).
func localSocket(host string) bool { return host == "" || strings.HasPrefix(host, "unix://") }

// MeasureInfo is what a Docker engine has for runs: its CPUs and memory, and the free space of its data
// root read with free when the engine is not VM-backed (a VM's data root is not a path here). Anything
// it cannot say is 0.
func MeasureInfo(info system.Info, dockerHost string, free func(path string) int64) types.RunnerAmounts {
	out := types.RunnerAmounts{CPUMillis: int64(max(0, info.NCPU)) * 1000, MemoryMiB: max(0, info.MemTotal) >> 20}
	if !VMBacked(info, dockerHost) && info.DockerRootDir != "" {
		out.DiskMiB = free(info.DockerRootDir)
	}
	return out
}

// FreeDiskMiB is the space free to an unprivileged writer under path, 0 when it cannot be read.
func FreeDiskMiB(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize) >> 20 // Bsize is int64 on Linux, uint32 on macOS
}
