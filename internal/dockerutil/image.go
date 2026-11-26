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
	"strings"

	"github.com/moby/moby/client"
)

// ImagePuller is the narrow Docker client surface PullImage needs, declaring
// the exact client.ImagePullResponse return type *client.Client needs to
// satisfy it; PullImage only uses its Close and Wait.
type ImagePuller interface {
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// localBuildNamespaces are the repositories Wardyn tags its own local builds under. No registry is meant to serve
// them, so a pull of one is never what the caller wants.
var localBuildNamespaces = []string{"wardyn-byoi/", "wardyn-devcontainer/", "wardyn-workspace/"}

// defaultRegistryHosts are the spellings of the default registry's host; the daemon reads docker.io/wardyn-byoi/x:1
// and wardyn-byoi/x:1 as one repository.
var defaultRegistryHosts = []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"}

// localBuildRef reports whether ref names a repository in one of localBuildNamespaces.
func localBuildRef(ref string) bool {
	for _, host := range defaultRegistryHosts {
		ref = strings.TrimPrefix(ref, host)
	}
	for _, ns := range localBuildNamespaces {
		if strings.HasPrefix(ref, ns) {
			return true
		}
	}
	return false
}

// PullImage pulls ref and waits for the stream to end, returning the error a registry reports inside
// it (a denied or missing image arrives there, after a nil ImagePull). errPrefix
// namespaces the wrapped errors per caller package (e.g. "docker" or
// "envbuild"), matching each package's existing error-message convention. A ref in one of Wardyn's own local build
// namespaces is refused before the daemon is asked: a missing local build is a failure, never a download.
func PullImage(ctx context.Context, cli ImagePuller, ref, errPrefix string) error {
	if localBuildRef(ref) {
		return fmt.Errorf("%s: pull %s: refused, it is in a namespace Wardyn builds locally and never pulls", errPrefix, ref)
	}
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
