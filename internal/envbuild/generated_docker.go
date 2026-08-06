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
	"path"
	"sort"
	"strings"
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
	// fixed caps; raise if a legitimate generated context ever needs
	// more than a handful of small files.
	maxGeneratedFiles     = 64
	maxGeneratedFileBytes = 1 << 20 // 1 MiB per file
)

// BuildFromDevcontainerFiles builds a workspace image from an in-memory set of
// generated devcontainer files (path -> content, e.g. workspacescan.GenerateDevcontainer's
// output) instead of a git repository. It stages the files into a throwaway
// host build context, bind-mounts that into the envbuilder container's
// workspace folder, and drives the SAME hardened build path as Build (network
// default-none, dropped caps, resource limits, force-remove, registry-push
// delivery, and the finalize stage that layers Wardyn's runner tools on).
//
// It reuses every hardening knob on Builder; the only differences from Build
// are the input surface (generated files vs. a git URL) and the delivery of
// that input (a read-safe host bind-mount vs. envbuilder's own clone).
//
// The git-free local-context build's real-daemon behaviour is smoke-tested by
// its own sibling, not just borrowed from the git path's coverage:
// TestBuildFromDevcontainerFiles_BakesAgentCLI (agent_tool_integration_test.go,
// WARDYN_TEST_DOCKER=1) drives this exact function end to end against a real
// daemon and execs the delivered image's claude binary.
//
// logSink, when non-nil, receives this build's output; nil falls back to
// Builder.DefaultLogSink (runBuildAndFinalize's own fallback — see there).
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

	// Deliver the generated files as a TAR streamed into the created build
	// container (CopyToContainer) rather than a bind mount: a bind names a
	// HOST path, and this code may run inside a containerized wardynd whose
	// own /tmp the host daemon cannot see — the old MkdirTemp+bind staged an
	// empty workspace there and every containerized build died with exit 1.
	// The tar is our own generated, trusted content, assembled in memory.
	tarCtx, err := generatedFilesTar(files, localContextWorkspaceFolder)
	if err != nil {
		return "", err
	}
	return b.runBuildAndFinalize(ctx, localBuildEnv(b.CacheRepo), nil, tarCtx, "/", logSink, outputTag)
}

// generatedFilesTar packs the generated files under destDir into an in-memory
// tar suitable for CopyToContainer at "/" (paths are destDir-relative inside
// the archive so extraction lands them at destDir).
func generatedFilesTar(files map[string]string, destDir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := strings.TrimPrefix(destDir, "/")
	// Deterministic order — nice for tests and reproducible archives.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
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

// localBuildEnv builds the envbuilder environment for a git-free local-context
// build: no ENVBUILDER_GIT_URL (so envbuilder builds from the mounted workspace
// folder rather than cloning), the registry PUSH vars, and ENVBUILDER_INIT_SCRIPT
// so the container exits after the push (see buildEnv). There is no
// ENVBUILDER_IMAGE_DEST (the var does not exist upstream); delivery is the
// registry push, and Wardyn finalizes the local tag afterwards. Pure, so it is
// unit-testable.
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
