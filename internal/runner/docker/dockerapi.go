// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package docker implements the runner.Runner contract on top of the Docker
// Engine API. It enforces Wardyn's L0 structural-egress invariant (see
// ARCHITECTURE.md invariant 3): the agent container is created with
// NetworkMode "none" (no default route) and reaches the network only through
// the wardyn-proxy sidecar, which it can address solely on a per-run
// user-defined *internal* network. Because Internal=true networks are created
// with no gateway, that network cannot route off-host even if a route existed;
// the proxy alone bridges to the wardyn-internal network that reaches the
// control plane. The agent therefore has exactly one egress path: proxy:3128.
package docker

import (
	"context"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// dockerAPI is the narrow slice of the Docker client the driver uses. It
// exists so lifecycle logic can be exercised against a fake with no daemon
// present. *client.Client satisfies it directly (asserted below).
//
// The moby v29 client redesigned every method onto an options/result shape:
// the arguments collapse into one options struct and the return values into a
// result struct (e.g. ContainerCreate takes ContainerCreateOptions and returns
// ContainerCreateResult). The interface mirrors that shape exactly so the real
// *client.Client keeps satisfying it.
type dockerAPI interface {
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)

	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	// ImageInspect resolves a ref (including a digest-pinned repo@sha256:... ref,
	// which the tag-shaped ImageList reference filter cannot match) to check
	// local presence without a registry round-trip.
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)

	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)

	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(ctx context.Context, containerID string, options client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	// ContainerWait blocks until the container reaches condition and yields its
	// exit code. Used by Wait for EXEC-LESS runtimes (krun microVMs), whose agent
	// workload runs as the container's MAIN process rather than a docker exec.
	// The v29 client folds the old (status, error) channel pair into a single
	// ContainerWaitResult carrying both channels (.Result / .Error).
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult

	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecStart(ctx context.Context, execID string, options client.ExecStartOptions) (client.ExecStartResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	// ExecResize resizes the PTY of an interactive exec. Used by an attach
	// Session to honour client window-size changes.
	ExecResize(ctx context.Context, execID string, options client.ExecResizeOptions) (client.ExecResizeResult, error)
}

// the real client must implement our slice.
var _ dockerAPI = (*client.Client)(nil)

// isNotFound reports whether err is a Docker "no such object" error. Teardown
// paths treat this as success so Stop/Kill are idempotent on a gone sandbox.
// The moby v29 client surfaces 404s as containerd errdefs errors (it dropped
// the old client.IsErrNotFound helper), so we classify via errdefs.IsNotFound —
// which matches both an errdefs.ErrNotFound wrap and any error implementing the
// NotFound() marker method.
func isNotFound(err error) bool {
	return err != nil && errdefs.IsNotFound(err)
}
