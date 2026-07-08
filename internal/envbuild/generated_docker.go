// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild is documented in doc.go.
package envbuild

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

const (
	// localContextWorkspaceFolder is where the generated build context is
	// bind-mounted inside the envbuilder container. It is envbuilder's default
	// workspace folder; we ALSO set ENVBUILDER_WORKSPACE_FOLDER to it explicitly
	// so we don't silently depend on that default staying put. With no
	// ENVBUILDER_GIT_URL, envbuilder builds from whatever devcontainer.json /
	// Dockerfile it finds in this folder (docs/using-local-files.md), so we get
	// a git-free build straight from the generated files.
	localContextWorkspaceFolder = "/workspaces/empty"

	// Bounds on the caller-supplied generated context, mirroring builder.go's
	// input hardening. GenerateDevcontainer emits a single small file; these
	// caps just keep a hostile/oversized map from staging a huge context.
	// ponytail: fixed caps; raise if a legitimate generated context ever needs
	// more than a handful of small files.
	maxGeneratedFiles     = 64
	maxGeneratedFileBytes = 1 << 20 // 1 MiB per file
)

// BuildFromDevcontainerFiles builds a workspace image from an in-memory set of
// generated devcontainer files (path -> content, e.g. workspacescan.GenerateDevcontainer's
// output) instead of a git repository. It stages the files into a throwaway
// host build context, bind-mounts that into the envbuilder container's
// workspace folder, and drives the SAME hardened build path as Build (network
// default-none, dropped caps, resource limits, force-remove, daemonless
// CacheRepo push vs. the opt-in docker.sock fallback).
//
// It reuses every hardening knob on Builder; the only differences from Build
// are the input surface (generated files vs. a git URL) and the delivery of
// that input (a read-safe host bind-mount vs. envbuilder's own clone).
//
// UNDER-VERIFIED SEAM (documented in the report): the git-free local-context
// build is exercised here through the same create/start/wait-for-exit lifecycle
// Build uses, but envbuilder's build-then-exit behaviour on a real daemon is
// only smoke-tested for the git path (integration_test.go, WARDYN_TEST_DOCKER=1).
// A sibling real-daemon smoke test for this path is the remaining verification.
func (b *Builder) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string) (imageRef string, err error) {
	if outputTag == "" {
		return "", fmt.Errorf("envbuild: outputTag is required")
	}
	if err := validateGeneratedTag(outputTag); err != nil {
		return "", err
	}
	if err := validateGeneratedFiles(files); err != nil {
		return "", err
	}

	// Same image-delivery decision as Build: default to the safe daemonless
	// registry PUSH path; only fall back to the dangerous docker.sock mount when
	// explicitly opted in; otherwise FAIL CLOSED.
	pushMode := b.CacheRepo != ""
	if !pushMode && !b.AllowDockerSock {
		return "", fmt.Errorf("envbuild: refusing to build: no CacheRepo configured " +
			"(registry push mode) and the docker.sock fallback is disabled. Set a " +
			"cache/registry repo (recommended) or, for trusted single-host dev only, " +
			"WARDYN_ENVBUILD_ALLOW_DOCKER_SOCK=1")
	}

	// Stage the generated files into a throwaway host directory that becomes the
	// build context. It holds only our own generated, trusted content and is
	// always removed on return.
	ctxDir, err := os.MkdirTemp("", "wardyn-envbuild-ctx-")
	if err != nil {
		return "", fmt.Errorf("envbuild: create build context dir: %w", err)
	}
	defer os.RemoveAll(ctxDir)
	if err := writeGeneratedFiles(ctxDir, files); err != nil {
		return "", err
	}

	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := b.ensureImage(ctx, b.EnvbuilderImage); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image: b.EnvbuilderImage,
		Env:   localBuildEnv(outputTag, b.CacheRepo),
	}
	hostCfg := b.hardenedHostConfig(pushMode)
	// Layer the generated build context on top of whatever hardenedHostConfig
	// already bound (nil in push mode; the docker.sock in the opt-in path). The
	// host path is a throwaway temp dir of our own generated content, so mounting
	// it does not widen the untrusted-code blast radius the way a real host path
	// would.
	hostCfg.Binds = append(hostCfg.Binds, ctxDir+":"+localContextWorkspaceFolder)

	created, err := b.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("envbuild: create build container: %w", err)
	}
	containerID := created.ID

	// Always force-remove the build container even on cancellation or panic.
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = b.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
	}()

	if err := b.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("envbuild: start build container: %w", err)
	}

	waitCh, errCh := b.cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("envbuild: build cancelled or timed out: %w", ctx.Err())
	case waitErr := <-errCh:
		return "", fmt.Errorf("envbuild: waiting for build container: %w", waitErr)
	case resp := <-waitCh:
		if resp.Error != nil {
			return "", fmt.Errorf("envbuild: build container error: %s", resp.Error.Message)
		}
		if resp.StatusCode != 0 {
			return "", fmt.Errorf("envbuild: build failed with exit code %d", resp.StatusCode)
		}
	}
	return outputTag, nil
}

// localBuildEnv builds the envbuilder environment for a git-free local-context
// build: no ENVBUILDER_GIT_URL (so envbuilder builds from the mounted workspace
// folder rather than cloning), the image destination, and — when a CacheRepo is
// set — the daemonless registry push vars. Pure, so it is unit-testable.
func localBuildEnv(outputTag, cacheRepo string) []string {
	env := []string{
		"ENVBUILDER_IMAGE_DEST=" + outputTag,
		"ENVBUILDER_WORKSPACE_FOLDER=" + localContextWorkspaceFolder,
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

// validateGeneratedFiles enforces the trust-boundary checks on the caller's
// path -> content map before any of it is written to disk: a bounded file
// count, per-file size cap, repo-relative paths only (no absolute/backslash/
// ".." traversal, reusing Build's validateRepoRelPath), and the presence of a
// devcontainer/Dockerfile envbuilder can actually build.
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

// writeGeneratedFiles materialises the validated files under root. Paths are
// already validated as repo-relative and traversal-free; the extra containment
// check is defense-in-depth against a path that slips past validation.
func writeGeneratedFiles(root string, files map[string]string) error {
	cleanRoot := filepath.Clean(root)
	for _, p := range slices.Sorted(maps.Keys(files)) { // sorted for deterministic writes
		full := filepath.Join(cleanRoot, filepath.FromSlash(p))
		if full != cleanRoot && !strings.HasPrefix(full, cleanRoot+string(filepath.Separator)) {
			return fmt.Errorf("envbuild: generated file %q escapes the build context", p)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("envbuild: create dir for %q: %w", p, err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			return fmt.Errorf("envbuild: write %q: %w", p, err)
		}
	}
	return nil
}
