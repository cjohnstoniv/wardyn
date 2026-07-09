// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/envbuild"
)

// envBuilderAdapter adapts *envbuild.Builder to api.ImageBuilder. The envbuild
// package is build-tagged "docker" (it imports the docker client), so this seam
// keeps the control-plane default build free of target-specific code.
type envBuilderAdapter struct {
	b *envbuild.Builder
}

var _ api.ImageBuilder = envBuilderAdapter{}

func (e envBuilderAdapter) BuildDevcontainer(ctx context.Context, repoURL, ref, outputTag string) (string, error) {
	return e.b.Build(ctx, envbuild.BuildSpec{
		RepoURL:        repoURL,
		Ref:            ref,
		OutputImageTag: outputTag,
	})
}

// BuildFromDevcontainerFiles builds a per-workspace image from generated
// devcontainer files (workspacescan.GenerateDevcontainer's output) via the
// git-free local-context envbuilder path. Same hardened builder, no git URL.
func (e envBuilderAdapter) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string) (string, error) {
	return e.b.BuildFromDevcontainerFiles(ctx, files, outputTag)
}

// newEnvBuilder constructs the devcontainer image builder. Compiled only with
// `-tags docker`. The builder runs daemonless: envbuilder pushes to the registry
// (cacheRepo) and Wardyn finalizes the image locally, so a missing cacheRepo
// fails closed.
//
// ponytail: the third parameter is the retired docker.sock knob
// (WARDYN_ENVBUILD_ALLOW_DOCKER_SOCK / -envbuild-allow-docker-sock). It is
// IGNORED — kaniko-based envbuilder never talks to dockerd, so the socket mount
// had zero function and was pure host-escape exposure. The signature is kept
// because main.go still passes the flag through; the flag is now a no-op.
func newEnvBuilder(envbuilderImage, cacheRepo string, _ bool) (api.ImageBuilder, error) {
	b, err := envbuild.New(envbuilderImage, cacheRepo)
	if err != nil {
		return nil, err
	}
	return envBuilderAdapter{b: b}, nil
}
