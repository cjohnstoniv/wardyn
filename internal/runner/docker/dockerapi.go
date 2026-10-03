// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package docker implements the runner.Runner contract on the Docker Engine API.
//
// INVARIANT (ARCHITECTURE.md invariant 3, L0 structural egress): the agent container is created with
// NetworkMode "none" and reaches the network only through the wardyn-proxy sidecar, addressable solely on
// a per-run internal network. Internal=true networks have no gateway, so it can't route off-host even if a
// route existed; only the proxy bridges to wardyn-internal. The agent has exactly one egress path: proxy:3128.
package docker

import (
	"context"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// dockerAPI is the narrow slice of the Docker client the driver uses, so lifecycle logic can run against a
// fake with no daemon present. *client.Client satisfies it directly (asserted below); the moby v29 client's
// options/result shape (args collapse into one options struct, returns into a result struct) is mirrored
// exactly for that.
type dockerAPI interface {
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)

	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	// ImageInspect resolves a digest-pinned repo@sha256:... ref (which ImageList's tag filter can't match)
	// to check local presence without a registry round-trip.
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImageRemove(ctx context.Context, imageID string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error) // reclaims a workspace image tag superseded by rescan/edit/delete

	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error) // reads back the subnet the daemon picked for a per-run network
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)

	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(ctx context.Context, containerID string, options client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) // attaches to a created proxy's stdin, the one way its config reaches it
	// ContainerPause / ContainerUnpause back runner.Freezer: pause the agent's process in place — memory,
	// disk and established TCP connections keep state — without stopping or removing it.
	ContainerPause(ctx context.Context, containerID string, options client.ContainerPauseOptions) (client.ContainerPauseResult, error)
	ContainerUnpause(ctx context.Context, containerID string, options client.ContainerUnpauseOptions) (client.ContainerUnpauseResult, error)
	// ContainerStats backs runner.ActivitySampler: one non-streaming read of the agent container's CPU
	// counters, with the daemon's own earlier sample so the reading is a rate. Nothing runs in the sandbox.
	ContainerStats(ctx context.Context, containerID string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	// ContainerWait blocks for a terminal condition and yields the exit code; used by Wait for EXEC-LESS
	// runtimes (krun microVMs) whose agent workload is the container's MAIN process, not a docker exec.
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	// CopyToContainer extracts a tar archive as ROOT; delivers SandboxSpec.ManagedFiles between
	// ContainerCreate and ContainerStart, the only window a file the agent can't modify can land unraced.
	CopyToContainer(ctx context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	ContainerStatPath(ctx context.Context, containerID string, options client.ContainerStatPathOptions) (client.ContainerStatPathResult, error) // checks a managed file's target directory exists before copy
	CopyFromContainer(ctx context.Context, containerID string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) // reads /etc, /etc/passwd to vet an image without running anything
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)             // last lines of a proxy that exited at boot, so a config-load failure reports its cause

	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecStart(ctx context.Context, execID string, options client.ExecStartOptions) (client.ExecStartResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	ExecResize(ctx context.Context, execID string, options client.ExecResizeOptions) (client.ExecResizeResult, error) // honours attach-session PTY window-size changes

	// VolumeInspect / VolumeCreate back MANAGED user drives: a per-person named volume, created on first
	// use. VolumeRemove is reached from exactly ONE caller — ReclaimDrive, the operator's explicit verb.
	// SECURITY/invariant: no teardown, sweep or run path may call it — a drive outlives every run that
	// mounts it, so an automatic reclaim would be data loss, not cleanup.
	VolumeInspect(ctx context.Context, volumeID string, options client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeRemove(ctx context.Context, volumeID string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

var _ dockerAPI = (*client.Client)(nil) // the real client must implement our slice

// isNotFound reports whether err is a Docker "no such object" error; teardown paths treat this as success
// so Stop/Kill are idempotent on a gone sandbox. Classified via errdefs.IsNotFound since the v29 client
// surfaces 404s as containerd errdefs errors and dropped the old client.IsErrNotFound helper.
func isNotFound(err error) bool {
	return err != nil && errdefs.IsNotFound(err)
}
