// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"io"

	"github.com/moby/moby/client"
)

// envbuilderDockerAPI is the narrow slice of the Docker client Builder needs.
// The interface is defined locally (not imported from internal/runner/docker)
// so the dep graph stays clean while *client.Client satisfies both.
type envbuilderDockerAPI interface {
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error)
	// ImageInspect reads the base's ONBUILD triggers before using it as a FROM.
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	// ImageRemove best-effort drops the per-build base tag after wrapping.
	ImageRemove(ctx context.Context, ref string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error)

	// DistributionInspect asks the registry (through the daemon, with its proxy and mirrors) for a ref's digest.
	DistributionInspect(ctx context.Context, imageRef string, options client.DistributionInspectOptions) (client.DistributionInspectResult, error)

	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	// CopyFromContainer reads the pin file out of the finished image through a container that is never started.
	CopyFromContainer(ctx context.Context, containerID string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	// CopyToContainer is the host-agnostic delivery for the build context: a
	// bind mount would name a HOST path, which a containerized wardynd lacks.
	CopyToContainer(ctx context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// the real client must implement our slice.
var _ envbuilderDockerAPI = (*client.Client)(nil)
