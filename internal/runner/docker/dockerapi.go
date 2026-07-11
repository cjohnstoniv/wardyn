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
	"io"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	dockerclient "github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerAPI is the narrow slice of the Docker client the driver uses. It
// exists so lifecycle logic can be exercised against a fake with no daemon
// present. *client.Client satisfies it directly (asserted below).
type dockerAPI interface {
	Info(ctx context.Context) (system.Info, error)

	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	// ImageInspect resolves a ref (including a digest-pinned repo@sha256:... ref,
	// which the tag-shaped ImageList reference filter cannot match) to check
	// local presence without a registry round-trip.
	ImageInspect(ctx context.Context, imageID string, opts ...dockerclient.ImageInspectOption) (image.InspectResponse, error)

	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkRemove(ctx context.Context, networkID string) error

	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerKill(ctx context.Context, containerID, signal string) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	// ContainerWait blocks until the container reaches condition and yields its
	// exit code. Used by Wait for EXEC-LESS runtimes (krun microVMs), whose agent
	// workload runs as the container's MAIN process rather than a docker exec.
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)

	ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (dockertypes.HijackedResponse, error)
	ContainerExecStart(ctx context.Context, execID string, options container.ExecStartOptions) error
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	// ContainerExecResize resizes the PTY of an interactive exec. Used by an
	// attach Session to honour client window-size changes.
	ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error
}

// the real client must implement our slice.
var _ dockerAPI = (*dockerclient.Client)(nil)

// isNotFound reports whether err is a Docker "no such object" error. Teardown
// paths treat this as success so Stop/Kill are idempotent on a gone sandbox.
func isNotFound(err error) bool {
	return err != nil && dockerclient.IsErrNotFound(err)
}
