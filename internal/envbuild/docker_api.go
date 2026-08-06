// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"io"

	"github.com/moby/moby/client"
)

// docker_api.go — the Docker client seam Builder depends on, split out of
// builder.go on its own so the interface and its real-client conformance pin
// read as one cohesive unit, independent of the build-orchestration logic
// that consumes it.

// envbuilderDockerAPI is the narrow slice of the Docker client that Builder
// needs. It mirrors the pattern in internal/runner/docker: the interface is
// defined locally (not imported from that package) so the dep graph stays
// clean while the same *client.Client satisfies both interfaces.
type envbuilderDockerAPI interface {
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error)
	// ImageInspect reads a local image's config; the wrap build uses it to read
	// the base's ONBUILD triggers before using it as a FROM (see assertWrapSafeBase).
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)

	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	// CopyToContainer streams a tar into a created (not-yet-started) container —
	// the host-agnostic delivery for the generated build context: a bind mount
	// names a HOST path, which a containerized wardynd's own /tmp is not.
	CopyToContainer(ctx context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// the real client must implement our slice.
var _ envbuilderDockerAPI = (*client.Client)(nil)
