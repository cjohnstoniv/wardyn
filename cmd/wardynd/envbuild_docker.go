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
// `-tags docker`. allowDockerSock opts into the dangerous local-daemon path; by
// default the builder runs daemonless (registry push mode via cacheRepo) or
// fails closed. See envbuild.Builder.AllowDockerSock.
func newEnvBuilder(envbuilderImage, cacheRepo string, allowDockerSock bool) (api.ImageBuilder, error) {
	b, err := envbuild.New(envbuilderImage, cacheRepo)
	if err != nil {
		return nil, err
	}
	b.AllowDockerSock = allowDockerSock
	return envBuilderAdapter{b: b}, nil
}
