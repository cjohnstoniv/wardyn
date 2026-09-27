// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
)

const (
	// localContextWorkspaceFolder is envbuilder's default workspace folder, also
	// set via ENVBUILDER_WORKSPACE_FOLDER; with no ENVBUILDER_GIT_URL, it builds
	// from whatever devcontainer.json/Dockerfile is here.
	localContextWorkspaceFolder = "/workspaces/empty"

	// Bounds on the caller-supplied generated context (mirrors builder.go's
	// input hardening) to keep a hostile/oversized map from staging a huge context.
	maxGeneratedFiles     = 64
	maxGeneratedFileBytes = 1 << 20 // 1 MiB per file
)

// BuildFromDevcontainerFiles builds a workspace image from an in-memory set
// of generated devcontainer files (path -> content) instead of a git repo,
// driving the SAME hardened build path as Build (network default-none,
// dropped caps, resource limits, force-remove, registry-push, finalize).
//
// TestBuildFromDevcontainerFiles_BakesAgentCLI (WARDYN_TEST_DOCKER=1)
// smoke-tests this; logSink, non-nil, receives output (else Builder.DefaultLogSink).
func (b *Builder) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string, logSink io.Writer) (imageRef string, err error) {
	if outputTag == "" {
		return "", fmt.Errorf("envbuild: outputTag is required")
	}
	if err := validateGeneratedTag(outputTag); err != nil {
		return "", err
	}
	if err := validateGeneratedFiles(files); err != nil {
		return "", err
	}

	// Delivered as a TAR via CopyToContainer, not a bind mount — a bind names a
	// HOST path invisible from a containerized wardynd's own /tmp.
	tarCtx, err := generatedFilesTar(files, localContextWorkspaceFolder)
	if err != nil {
		return "", err
	}
	// A fresh per-build push ref: see Builder.newPushRef.
	pushRepo, buildTag := b.newPushRef()
	return b.runBuildAndFinalize(ctx, localBuildEnv(pushRepo), nil, tarCtx, "/", logSink, outputTag, buildTag)
}

// generatedFilesTar packs files under destDir into an in-memory tar for
// CopyToContainer at "/" (archive paths are destDir-relative).
func generatedFilesTar(files map[string]string, destDir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := strings.TrimPrefix(destDir, "/")
	// Deterministic order — nice for tests and reproducible archives.
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{
			Name: path.Join(base, name),
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("envbuild: tar build context: %w", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, fmt.Errorf("envbuild: tar build context: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("envbuild: tar build context: %w", err)
	}
	return &buf, nil
}

// localBuildEnv builds the envbuilder environment for a git-free local build
// (no ENVBUILDER_GIT_URL; mounted workspace, not a clone) plus registry PUSH vars.
func localBuildEnv(cacheRepo string) []string {
	env := []string{
		"ENVBUILDER_WORKSPACE_FOLDER=" + localContextWorkspaceFolder,
		"ENVBUILDER_INIT_SCRIPT=exit 0",
	}
	if cacheRepo != "" {
		env = append(env, "ENVBUILDER_CACHE_REPO="+cacheRepo)
		env = append(env, "ENVBUILDER_PUSH_IMAGE=true")
	}
	return env
}

// validateGeneratedTag bounds and control-char-checks the output tag before it
// reaches envbuilder's environment, mirroring Build's input hardening.
func validateGeneratedTag(tag string) error {
	if len(tag) > maxBuildInputLen {
		return fmt.Errorf("envbuild: outputTag exceeds the %d-byte input bound", maxBuildInputLen)
	}
	if strings.ContainsAny(tag, " \t\r\n\x00") {
		return fmt.Errorf("envbuild: outputTag contains illegal whitespace/control characters")
	}
	return nil
}

// validateGeneratedFiles enforces trust-boundary checks on the caller's
// path -> content map: bounded file count, per-file size cap, repo-relative
// paths only, and presence of a devcontainer/Dockerfile to build.
func validateGeneratedFiles(files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("envbuild: no generated files to build")
	}
	if len(files) > maxGeneratedFiles {
		return fmt.Errorf("envbuild: too many generated files (%d > %d)", len(files), maxGeneratedFiles)
	}
	buildable := false
	for p, content := range files {
		if err := validateRepoRelPath("generated file path", p); err != nil {
			return err
		}
		if len(content) > maxGeneratedFileBytes {
			return fmt.Errorf("envbuild: generated file %q exceeds the %d-byte cap", p, maxGeneratedFileBytes)
		}
		switch {
		case p == ".devcontainer/devcontainer.json",
			p == ".devcontainer.json",
			p == "Dockerfile",
			strings.HasSuffix(p, "/devcontainer.json"),
			strings.HasSuffix(p, "/Dockerfile"):
			buildable = true
		}
	}
	if !buildable {
		return fmt.Errorf("envbuild: generated files contain no devcontainer.json or Dockerfile for envbuilder to build")
	}
	return nil
}
