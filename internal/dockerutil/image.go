// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package dockerutil holds tiny helpers shared by the Docker-backed packages
// (internal/runner/docker, internal/envbuild) that each define their own
// narrow slice of the Docker client interface.
package dockerutil

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/image"
)

// ImagePuller is the narrow Docker client surface PullImage needs.
type ImagePuller interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
}

// PullImage pulls ref, draining and discarding the stream. errPrefix
// namespaces the wrapped errors per caller package (e.g. "docker" or
// "envbuild"), matching each package's existing error-message convention.
func PullImage(ctx context.Context, cli ImagePuller, ref, errPrefix string) error {
	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("%s: pull %s: %w", errPrefix, ref, err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("%s: drain pull %s: %w", errPrefix, ref, err)
	}
	return nil
}
