// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// The live cases below drive a real daemon, a throwaway registry and the public registries the
// generated recipe reaches. They need the same environment as
// TestBuildFromDevcontainerFiles_BakesAgentCLI, and skip only when it is absent.

func liveBuilder(t *testing.T) *Builder {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker pin cases")
	}
	cacheRepo, toolsDir := os.Getenv("WARDYN_TEST_CACHE_REPO"), os.Getenv("WARDYN_TEST_TOOLS_DIR")
	if cacheRepo == "" || toolsDir == "" {
		t.Skip("set WARDYN_TEST_CACHE_REPO and WARDYN_TEST_TOOLS_DIR to run the real-Docker pin cases")
	}
	b, err := New("", cacheRepo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.BuildTimeout = 25 * time.Minute
	b.ToolsDir = toolsDir
	b.BuildNetwork = "host"
	if n := os.Getenv("WARDYN_ENVBUILD_TEST_NETWORK"); n != "" {
		b.BuildNetwork = n
	}
	return b
}

func liveProfile() workspacescan.WorkspaceProfile {
	return workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
}

func removeImageOnCleanup(t *testing.T, tag string) {
	t.Helper()
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })
}

// liveBuild builds the generated files for pins and returns what the builder reported with its log.
func liveBuild(ctx context.Context, t *testing.T, b *Builder, pins workspacescan.Pins, tag string) (Built, string, error) {
	t.Helper()
	files, err := workspacescan.GenerateDevcontainer(liveProfile(), pins)
	if err != nil {
		t.Fatalf("GenerateDevcontainer: %v", err)
	}
	var logs bytes.Buffer
	built, err := b.BuildFromDevcontainerFiles(ctx, files, tag, &logs)
	return built, logs.String(), err
}

// pinsOf is what a record of a build keeps: the pins the build used and the installer its pin file reported.
func pinsOf(t *testing.T, pins workspacescan.Pins, pinFile []byte) types.ImageBuildInputs {
	t.Helper()
	in, err := workspacescan.GeneratedInputs(liveProfile(), pins)
	if err != nil {
		t.Fatalf("GeneratedInputs: %v", err)
	}
	pin, ok := workspacescan.ParseInstallerPin(pinFile)
	if !ok {
		t.Fatalf("the build's pin file %q does not parse", pinFile)
	}
	for i := range in.Installers {
		in.Installers[i].Version, in.Installers[i].Platform, in.Installers[i].SHA256 = pin.Version, pin.Platform, pin.SHA256
	}
	return in
}

func TestLive_PinnedBuildRebuildsWithEqualInputs(t *testing.T) {
	b := liveBuilder(t)
	ctx, cancel := context.WithTimeout(t.Context(), 55*time.Minute)
	defer cancel()
	const first, second = "wardyn-envbuild-pintest:first", "wardyn-envbuild-pintest:second"
	removeImageOnCleanup(t, first)
	removeImageOnCleanup(t, second)

	// Resolve with the real daemon.
	unpinned, err := workspacescan.GeneratedInputs(liveProfile(), workspacescan.Pins{})
	if err != nil {
		t.Fatal(err)
	}
	pins := workspacescan.Pins{Features: map[string]string{}}
	if pins.BaseDigest, err = b.RemoteDigest(ctx, unpinned.Base.Ref); err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	for _, f := range unpinned.Features {
		if pins.Features[f.Ref], err = b.RemoteDigest(ctx, f.Ref); err != nil {
			t.Fatalf("resolve feature %s: %v", f.Ref, err)
		}
	}
	t.Logf("resolved base %s, features %v", pins.BaseDigest, pins.Features)

	// Build pinned, then keep what the record would.
	built1, logs1, err := liveBuild(ctx, t, b, pins, first)
	if err != nil {
		t.Fatalf("first build: %v\nlogs:\n%s", err, logs1)
	}
	record := pinsOf(t, pins, built1.PinFile)
	t.Logf("first build's pin file: %q", built1.PinFile)

	// The host loses the image; the record's pins rebuild it.
	if out, err := exec.CommandContext(ctx, "docker", "image", "rm", "-f", first).CombinedOutput(); err != nil {
		t.Fatalf("docker image rm: %v\n%s", err, out)
	}
	if _, found, err := b.LocalImageID(ctx, first); err != nil || found {
		t.Fatalf("the image is still on the host: found=%v err=%v", found, err)
	}
	built2, logs2, err := liveBuild(ctx, t, b, workspacescan.PinsFromInputs(record), second)
	if err != nil {
		t.Fatalf("rebuild from the record: %v\nlogs:\n%s", err, logs2)
	}
	rebuilt, err := workspacescan.GeneratedInputs(liveProfile(), workspacescan.PinsFromInputs(record))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(built1.PinFile, built2.PinFile) {
		t.Errorf("the rebuild's pin file %q differs from the first build's %q", built2.PinFile, built1.PinFile)
	}
	// The files hash is left out: a pinned installer's RUN is, by design, not the text the first build ran.
	rebuilt.Generator, record.Generator = nil, nil
	if !reflect.DeepEqual(rebuilt, record) {
		t.Errorf("rebuilt inputs %+v differ from the record's %+v", rebuilt, record)
	}
	if rebuilt.Base.Digest != pins.BaseDigest || rebuilt.Installers[0].Version == "" {
		t.Errorf("the rebuilt inputs are not pinned: %+v", rebuilt)
	}
}

// The negative control for the feature pin: envbuilder 1.3.0 does not log feature fetches, so the proof
// that a digest-keyed feature is fetched by digest is that an unknown digest fails the build.
func TestLive_FeaturePinnedToAnUnknownDigestFails(t *testing.T) {
	b := liveBuilder(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	const tag = "wardyn-envbuild-pinneg:latest"
	removeImageOnCleanup(t, tag)

	pins := workspacescan.Pins{Features: map[string]string{"ghcr.io/devcontainers/features/go:1": "sha256:" + strings.Repeat("0", 64)}}
	_, logs, err := liveBuild(ctx, t, b, pins, tag)
	if err == nil {
		t.Fatalf("a feature pinned to an unknown digest built:\n%s", logs)
	}
	if !strings.Contains(logs, "MANIFEST_UNKNOWN") {
		t.Fatalf("the build failed (%v) without naming the unknown manifest:\n%s", err, logs)
	}
}

// The adapter maps this answer to api.ErrRemoteDigestNotFound, so it has to be the daemon's not-found
// and not an unreachable-registry error.
func TestLive_RemoteDigestUnknownManifestIsNotFound(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker pin cases")
	}
	b, err := New("", "unused.invalid/repo")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	d, err := b.RemoteDigest(ctx, "ghcr.io/devcontainers/features/go:1")
	if err != nil || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("a known feature: digest %q, err %v", d, err)
	}
	_, err = b.RemoteDigest(ctx, "ghcr.io/devcontainers/features/go@sha256:"+strings.Repeat("0", 64))
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("an unknown manifest: err = %v, want a not-found error", err)
	}
}
