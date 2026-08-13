// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestBuildFromDevcontainerFiles_BakesAgentCLI is the only thing that can
// actually prove the agent-CLI bake: it drives the REAL generated-devcontainer
// build end to end and then EXECS the claimed binary in the delivered image.
//
// A unit test on the generated JSON cannot prove this, and that is exactly how
// the bake shipped broken once: a devcontainer `onCreateCommand` is a lifecycle
// hook envbuilder runs AFTER executor.DoBuild + DoPush, so it never became a
// layer of the pushed image Wardyn delivers, and the runner boots the finalized
// tag directly (no devcontainer CLI) so it never ran at run time either. Only
// `docker run <tag> claude --version` distinguishes a real bake from inert JSON.
//
// It also closes generated_docker.go's "UNDER-VERIFIED SEAM" note: this is the
// sibling real-daemon smoke test for the git-free local-context path.
//
// Preconditions are the same as TestBuild_SmokeDockerd's, minus the git daemon
// (there is no clone here — the context is generated in memory).
func TestBuildFromDevcontainerFiles_BakesAgentCLI(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker agent-CLI bake test")
	}
	cacheRepo := os.Getenv("WARDYN_TEST_CACHE_REPO")
	if cacheRepo == "" {
		t.Skip("set WARDYN_TEST_CACHE_REPO=<registry/repo> (a writable registry) to run the bake test")
	}
	toolsDir := os.Getenv("WARDYN_TEST_TOOLS_DIR")
	if toolsDir == "" {
		t.Skip("set WARDYN_TEST_TOOLS_DIR=<dir with the runner tools> to run the bake test")
	}

	// The exact production input: the profile a scan derives. Nothing
	// test-specific is injected — the standard agent-tool install is baked
	// unconditionally now (no integration wiring), so if this image carries
	// claude, so does the one a workspace build produces.
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	files, err := workspacescan.GenerateDevcontainer(profile)
	if err != nil {
		t.Fatalf("GenerateDevcontainer: %v", err)
	}
	t.Logf("generated devcontainer.json:\n%s", files[".devcontainer/devcontainer.json"])

	tag := "wardyn-envbuild-baketest:latest"
	b, err := New("", cacheRepo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.BuildTimeout = 25 * time.Minute
	b.ToolsDir = toolsDir
	// Same reasoning as TestBuild_SmokeDockerd: only host networking puts the
	// build container where the loopback registry (and ghcr/mcr) are reachable.
	buildNet := os.Getenv("WARDYN_ENVBUILD_TEST_NETWORK")
	if buildNet == "" {
		buildNet = "host"
	}
	b.BuildNetwork = buildNet

	var logs bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 27*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		_ = exec.Command("docker", "image", "rm", "-f", tag).Run()
	})

	ref, err := b.BuildFromDevcontainerFiles(ctx, files, tag, &logs)
	if err != nil {
		// Same two environmental preconditions TestBuild_SmokeDockerd documents:
		// classify those as SKIP, fail on anything else so a real regression is red.
		log := logs.String()
		switch {
		case strings.Contains(log, "operation not permitted") && strings.Contains(log, "chown"):
			t.Skipf("SKIP (unsupported daemon): kaniko could not chown while unpacking the base rootfs under CapDrop:ALL. err: %v", err)
		case strings.Contains(log, "connection refused"), strings.Contains(log, "dial tcp"):
			t.Skipf("SKIP (unsupported daemon): the build container could not reach the registry on host loopback. err: %v", err)
		default:
			t.Fatalf("BuildFromDevcontainerFiles: %v\nlogs:\n%s", err, log)
		}
	}

	// THE assertion: exec the binary the image claims to carry. `docker run`
	// rather than the client API so the failure output is the operator's own
	// reproduction command.
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", ref, "claude", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run --rm %s claude --version: %v\noutput: %s\nbuild logs:\n%s", ref, err, out, logs.String())
	}
	t.Logf("PROOF: docker run --rm %s claude --version => %s", ref, strings.TrimSpace(string(out)))
	if !strings.Contains(string(out), "Claude Code") {
		t.Errorf("claude --version output does not look like Claude Code: %q", out)
	}
}
