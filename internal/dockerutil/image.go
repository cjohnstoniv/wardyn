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

	"github.com/moby/moby/client"
)

// ImagePuller is the narrow Docker client surface PullImage needs, declaring
// the exact client.ImagePullResponse return type *client.Client needs to
// satisfy it; PullImage only uses its Close and Wait.
type ImagePuller interface {
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// PullImage pulls ref and waits for the stream to end, returning the error a registry reports inside
// it (a denied or missing image arrives there, after a nil ImagePull). errPrefix
// namespaces the wrapped errors per caller package (e.g. "docker" or
// "envbuild"), matching each package's existing error-message convention.
func PullImage(ctx context.Context, cli ImagePuller, ref, errPrefix string) error {
	rc, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("%s: pull %s: %w", errPrefix, ref, err)
	}
	defer rc.Close()
	if err := rc.Wait(ctx); err != nil {
		return fmt.Errorf("%s: pull %s: %w", errPrefix, ref, err)
	}
	return nil
}
