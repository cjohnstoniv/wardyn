// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// fakeToolsDir writes the required runner tool binaries into a temp dir so the
// finalize preflight passes without the real binaries present.
func fakeToolsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range requiredTools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake tool %q: %v", name, err)
		}
	}
	return dir
}

// newPushBuilder returns a Builder wired for the push+finalize path: a fake
// docker client, a CacheRepo (registry push mode), and a populated tools dir.
func newPushBuilder(t *testing.T, f *fakeEnvbuilderDocker) *Builder {
	t.Helper()
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")
	b.ToolsDir = fakeToolsDir(t)
	return b
}

// ---------------------------------------------------------------------------
// Pure-logic: spec -> env var mapping (no Docker daemon required)
// ---------------------------------------------------------------------------

func TestBuildEnv_RequiredVars(t *testing.T) {
	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc123",
	}
	env := buildEnv(spec, "")

	assertEnvValue(t, env, "ENVBUILDER_GIT_URL", spec.RepoURL)
	// ENVBUILDER_INIT_SCRIPT="exit 0" makes the container exit after the push.
	assertEnvValue(t, env, "ENVBUILDER_INIT_SCRIPT", "exit 0")
	// ENVBUILDER_IMAGE_DEST does not exist upstream and must never be set.
	if containsEnvKey(env, "ENVBUILDER_IMAGE_DEST") {
		t.Error("ENVBUILDER_IMAGE_DEST must not be set (it does not exist in envbuilder)")
	}
}

func TestBuildEnv_OptionalRefAndPath(t *testing.T) {
	spec := BuildSpec{
		RepoURL:          "https://github.com/example/repo",
		Ref:              "refs/heads/feature",
		DevcontainerPath: ".devcontainer/custom.json",
		OutputImageTag:   "wardyn-ws:abc123",
	}
	env := buildEnv(spec, "")

	assertEnvValue(t, env, "ENVBUILDER_GIT_REF", spec.Ref)
	assertEnvValue(t, env, "ENVBUILDER_DEVCONTAINER_PATH", spec.DevcontainerPath)
}

func TestBuildEnv_OmitsRefAndPathWhenEmpty(t *testing.T) {
	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc123",
		// Ref and DevcontainerPath intentionally empty
	}
	env := buildEnv(spec, "")

	if containsEnvKey(env, "ENVBUILDER_GIT_REF") {
		t.Error("ENVBUILDER_GIT_REF must be absent when Ref is empty")
	}
	if containsEnvKey(env, "ENVBUILDER_DEVCONTAINER_PATH") {
		t.Error("ENVBUILDER_DEVCONTAINER_PATH must be absent when DevcontainerPath is empty")
	}
}

func TestBuildEnv_CacheRepoSetsBothCacheVars(t *testing.T) {
	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc123",
	}
	cacheRepo := "registry.example.com/cache/repo"
	env := buildEnv(spec, cacheRepo)

	assertEnvValue(t, env, "ENVBUILDER_CACHE_REPO", cacheRepo)
	assertEnvValue(t, env, "ENVBUILDER_PUSH_IMAGE", "true")
}

func TestBuildEnv_NoCacheVarsWhenCacheRepoEmpty(t *testing.T) {
	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc123",
	}
	env := buildEnv(spec, "")

	if containsEnvKey(env, "ENVBUILDER_CACHE_REPO") {
		t.Error("ENVBUILDER_CACHE_REPO must be absent when CacheRepo is empty")
	}
	if containsEnvKey(env, "ENVBUILDER_PUSH_IMAGE") {
		t.Error("ENVBUILDER_PUSH_IMAGE must be absent when CacheRepo is empty")
	}
}

// ---------------------------------------------------------------------------
// pushedBaseRef / newPushRef: per-build push-ref composition (W20-record-image-2)
// ---------------------------------------------------------------------------

func TestPushedBaseRef_NoOverrideUsesCacheRepoPlusTag(t *testing.T) {
	b := newWithClient(newFakeEnvbuilderDocker(), "envbuilder:test", "registry.example.com/wardyn-cache")
	t.Setenv(envPushedRef, "")

	got := b.pushedBaseRef("deadbeef")
	want := "registry.example.com/wardyn-cache:deadbeef"
	if got != want {
		t.Errorf("pushedBaseRef(%q) = %q, want %q", "deadbeef", got, want)
	}
}

// The override names a REPOSITORY ADDRESS (e.g. compose's host-loopback path
// to the same registry the build container reaches by service name), not a
// fixed ref — the per-build tag must still compose on top, or every build
// would collapse back onto the override's one shared ref (W20-record-image-2
// again, just relocated to the override instead of CacheRepo).
func TestPushedBaseRef_EnvOverrideComposesWithPerBuildTag(t *testing.T) {
	b := newWithClient(newFakeEnvbuilderDocker(), "envbuilder:test", "registry.example.com/wardyn-cache")
	t.Setenv(envPushedRef, "127.0.0.1:5010/wardyn/devcontainers")

	got := b.pushedBaseRef("deadbeef")
	want := "127.0.0.1:5010/wardyn/devcontainers:deadbeef"
	if got != want {
		t.Errorf("pushedBaseRef(%q) = %q, want %q (override must compose with the per-build tag, not replace it)", "deadbeef", got, want)
	}
}

func TestNewPushRef_EmptyCacheRepoReturnsEmpty(t *testing.T) {
	b := newWithClient(newFakeEnvbuilderDocker(), "envbuilder:test", "")
	pushRepo, tag := b.newPushRef()
	if pushRepo != "" || tag != "" {
		t.Errorf("newPushRef() with empty CacheRepo = (%q, %q), want (\"\", \"\")", pushRepo, tag)
	}
}

// TestBuild_ConcurrentBuildsUsePerBuildPushRef pins W20-record-image-2 (HIGH,
// confinement bypass): two builds sharing a Builder — and therefore its one
// CacheRepo — must never resolve the SAME registry ref for envbuilder's push
// and finalize's pull-back. Before the fix, pushedBaseRef() was a pure
// function of b.CacheRepo alone, identical on every call regardless of which
// build made it, so a workspace-B push landing between workspace A's
// envbuilder-push and finalize-pull would get silently pulled and permanently
// tagged as workspace A's image (finalizeImage's pullBase=true pull is
// unconditional "pull fresh", so there is no re-validation step to catch it).
//
// This test needs no real goroutines to expose the bug: the OLD code computed
// the identical ref on every call regardless of timing, so two SEQUENTIAL
// builds already reproduce it deterministically — this FAILS against base
// 6d76911 (both builds' push/pull refs compare equal) and PASSES after the
// fix (each build gets its own random-tagged ref).
func TestBuild_ConcurrentBuildsUsePerBuildPushRef(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)
	t.Setenv(envPushedRef, "") // isolate from any host-set override

	specA := BuildSpec{RepoURL: "https://github.com/example/workspace-a", OutputImageTag: "wardyn-ws:a"}
	if _, err := b.Build(t.Context(), specA); err != nil {
		t.Fatalf("Build A: %v", err)
	}
	envA := append([]string(nil), f.lastEnv...)
	pulledA := append([]string(nil), f.pulledRefs...)

	specB := BuildSpec{RepoURL: "https://github.com/example/workspace-b", OutputImageTag: "wardyn-ws:b"}
	if _, err := b.Build(t.Context(), specB); err != nil {
		t.Fatalf("Build B: %v", err)
	}
	envB := f.lastEnv
	pulledB := f.pulledRefs

	pushRefA := envValue(envA, "ENVBUILDER_CACHE_REPO")
	pushRefB := envValue(envB, "ENVBUILDER_CACHE_REPO")
	if pushRefA == "" || pushRefB == "" {
		t.Fatalf("ENVBUILDER_CACHE_REPO missing: A=%q B=%q", pushRefA, pushRefB)
	}
	if pushRefA == pushRefB {
		t.Fatalf("workspace A and B pushed to the SAME ref %q: concurrent builds share one mutable FROM ref (W20-record-image-2)", pushRefA)
	}
	const wantPrefix = "registry.example.com/wardyn-cache"
	if !strings.HasPrefix(pushRefA, wantPrefix) || !strings.HasPrefix(pushRefB, wantPrefix) {
		t.Fatalf("push refs must still target CacheRepo %q: A=%q B=%q", wantPrefix, pushRefA, pushRefB)
	}

	// finalizeImage's pullBase=true path (pull the freshly-pushed base before
	// wrapping) must pull the SAME per-build ref THIS build was just told to
	// push to — never the other workspace's ref, and never a bare/shared one.
	lastPulledA := pulledA[len(pulledA)-1]
	lastPulledB := pulledB[len(pulledB)-1]
	if lastPulledA != pushRefA {
		t.Errorf("workspace A finalize pulled %q, want its own push ref %q", lastPulledA, pushRefA)
	}
	if lastPulledB != pushRefB {
		t.Errorf("workspace B finalize pulled %q, want its own push ref %q", lastPulledB, pushRefB)
	}
	if lastPulledA == lastPulledB {
		t.Fatalf("workspace B finalize pulled the SAME ref as workspace A (%q): B's image could be built from A's push (or vice versa)", lastPulledA)
	}
}

// BuildFromDevcontainerFiles (the git-free generated-context path) shares
// runBuildAndFinalize with Build, so it must get the same per-build isolation
// — this pins that it does not, say, fall back to a bare/shared CacheRepo of
// its own.
func TestBuildFromDevcontainerFiles_ConcurrentBuildsUsePerBuildPushRef(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)
	t.Setenv(envPushedRef, "")

	filesA := map[string]string{".devcontainer/devcontainer.json": `{"image":"golang:1.22"}`}
	if _, err := b.BuildFromDevcontainerFiles(t.Context(), filesA, "wardyn-ws:gen-a", nil); err != nil {
		t.Fatalf("BuildFromDevcontainerFiles A: %v", err)
	}
	pushRefA := envValue(f.lastEnv, "ENVBUILDER_CACHE_REPO")

	filesB := map[string]string{".devcontainer/devcontainer.json": `{"image":"golang:1.23"}`}
	if _, err := b.BuildFromDevcontainerFiles(t.Context(), filesB, "wardyn-ws:gen-b", nil); err != nil {
		t.Fatalf("BuildFromDevcontainerFiles B: %v", err)
	}
	pushRefB := envValue(f.lastEnv, "ENVBUILDER_CACHE_REPO")

	if pushRefA == "" || pushRefB == "" {
		t.Fatalf("ENVBUILDER_CACHE_REPO missing: A=%q B=%q", pushRefA, pushRefB)
	}
	if pushRefA == pushRefB {
		t.Fatalf("two BuildFromDevcontainerFiles calls pushed to the SAME ref %q (W20-record-image-2)", pushRefA)
	}
}

func TestBuildEnv_ValuesDontContainExtraEquals(t *testing.T) {
	// Verify that a value containing "=" is not mangled.
	spec := BuildSpec{
		RepoURL:        "https://host/path?foo=bar",
		OutputImageTag: "wardyn-ws:v1",
	}
	env := buildEnv(spec, "")

	got := envValue(env, "ENVBUILDER_GIT_URL")
	if got != spec.RepoURL {
		t.Errorf("ENVBUILDER_GIT_URL = %q, want %q", got, spec.RepoURL)
	}
}

// ---------------------------------------------------------------------------
// Validation: Build rejects specs missing required fields
// ---------------------------------------------------------------------------

func TestBuild_RejectsEmptyRepoURL(t *testing.T) {
	b := newWithClient(&fakeEnvbuilderDocker{}, "", "")
	_, err := b.Build(t.Context(), BuildSpec{OutputImageTag: "foo:bar"})
	if err == nil {
		t.Fatal("expected error for empty RepoURL")
	}
	if !strings.Contains(err.Error(), "RepoURL") {
		t.Errorf("error must mention RepoURL, got: %v", err)
	}
}

func TestBuild_RejectsEmptyOutputImageTag(t *testing.T) {
	b := newWithClient(&fakeEnvbuilderDocker{}, "", "")
	_, err := b.Build(t.Context(), BuildSpec{RepoURL: "https://example.com/repo"})
	if err == nil {
		t.Fatal("expected error for empty OutputImageTag")
	}
	if !strings.Contains(err.Error(), "OutputImageTag") {
		t.Errorf("error must mention OutputImageTag, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fake-Docker unit tests: verify Build paths without a real daemon
// ---------------------------------------------------------------------------

func TestBuild_SuccessPath(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}
	ref, err := b.Build(t.Context(), spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Build returns the finalize local tag (now genuinely produced by the
	// second-stage ImageBuild).
	if ref != spec.OutputImageTag {
		t.Errorf("imageRef = %q, want %q", ref, spec.OutputImageTag)
	}
	if f.createCalled == 0 {
		t.Error("expected ContainerCreate to be called")
	}
	if f.startCalled == 0 {
		t.Error("expected ContainerStart to be called")
	}
	// The finalize stage (H5) must run and tag the output image.
	if !f.imageBuildCalled {
		t.Error("expected finalize ImageBuild to be called")
	}
	if len(f.lastBuildTags) != 1 || f.lastBuildTags[0] != spec.OutputImageTag {
		t.Errorf("finalize build tags = %v, want [%q]", f.lastBuildTags, spec.OutputImageTag)
	}
	// Build container must be removed after success.
	if !f.removed {
		t.Error("build container must be removed after success")
	}
}

// TestBuild_StreamLogsDemuxesMultiplexedFrames pins the review fix (M5): the
// build container runs with no TTY, so a real ContainerLogs response
// multiplexes stdout/stderr behind an 8-byte frame header per chunk (see
// dockerFrame in fake_test.go) — a bare io.Copy fed those header bytes
// straight into the log sink as binary garbage. streamLogs must demux with
// stdcopy instead, so the sink only ever sees clean text.
func TestBuild_StreamLogsDemuxesMultiplexedFrames(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.logsBody = append(
		dockerFrame(1, "cloning repo\n"),
		dockerFrame(2, "warning: shallow clone\n")...,
	)
	b := newPushBuilder(t, f)
	var logSink bytes.Buffer
	b.DefaultLogSink = &logSink

	spec := BuildSpec{RepoURL: "https://github.com/example/repo", OutputImageTag: "wardyn-ws:log"}
	// L8: runBuildAndFinalize now joins the streaming goroutine before
	// finalize starts writing to the same sink, so by the time Build
	// returns the demuxed container log is guaranteed to already be here —
	// no sleep/poll needed to avoid a race in this assertion.
	if _, err := b.Build(t.Context(), spec); err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := logSink.String()
	if !strings.Contains(got, "cloning repo") || !strings.Contains(got, "warning: shallow clone") {
		t.Fatalf("log sink = %q, want both demuxed lines present", got)
	}
	// The raw frame header (stream-type byte + 3 zero padding bytes) must
	// never appear in the sink — that's exactly the garbage a bare io.Copy
	// would have let through.
	for _, header := range [][]byte{{1, 0, 0, 0}, {2, 0, 0, 0}} {
		if bytes.Contains([]byte(got), header) {
			t.Fatalf("log sink = %q, contains a raw Docker frame header %v — stdcopy demux did not run", got, header)
		}
	}
}

// Finalize failures (e.g. a missing COPY source at build time) surface as an
// error in the ImageBuild stream, not the ImageBuild return, so Build must
// detect them and fail.
func TestBuild_FailsWhenFinalizeBuildErrors(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.buildErr = "COPY failed: file not found in build context"
	b := newPushBuilder(t, f)

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err == nil {
		t.Fatal("expected Build to fail when the finalize build reports an error")
	}
	if !strings.Contains(err.Error(), "finalize") {
		t.Errorf("error must mention the finalize stage, got: %v", err)
	}
}

// Preflight: a build whose tools dir is missing a required runner binary must
// fail closed BEFORE running the build, not hand back a broken image tag.
func TestBuild_FailsClosedWhenToolMissing(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")
	dir := t.TempDir() // empty: no runner tools
	b.ToolsDir = dir

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err == nil {
		t.Fatal("expected Build to fail closed when a required runner tool is missing")
	}
	if f.createCalled != 0 {
		t.Error("must not create a build container when the tools preflight fails")
	}
}

func TestBuild_FailClosedOnNonZeroExit(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.exitCode = 1
	b := newPushBuilder(t, f)

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err == nil {
		t.Fatal("must fail closed on non-zero exit code")
	}
	if !strings.Contains(err.Error(), "exit code") {
		t.Errorf("error must mention exit code, got: %v", err)
	}
	// Build container must still be removed even on failure.
	if !f.removed {
		t.Error("build container must be removed even on failure")
	}
}

func TestBuild_PullsEnvbuilderImageWhenAbsent(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent = map[string]bool{} // no images pre-loaded
	b := newPushBuilder(t, f)

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !f.pulled("envbuilder:test") {
		t.Error("expected ImagePull to be called when image is absent")
	}
}

func TestBuild_SkipsPullWhenImagePresent(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent = map[string]bool{"envbuilder:test": true}
	b := newPushBuilder(t, f)

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Scoped to the envbuilder image: the finalize stage separately pulls the
	// freshly pushed base on purpose (see finalizeImage), which is not this
	// test's subject.
	if f.pulled("envbuilder:test") {
		t.Error("must not pull when image is already present")
	}
}

func TestBuild_ContainerEnvContainsRequiredVars(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	spec := BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		Ref:            "my-branch",
		OutputImageTag: "wardyn-ws:abc",
	}
	if _, err := b.Build(t.Context(), spec); err != nil {
		t.Fatalf("Build: %v", err)
	}

	env := f.lastEnv
	assertEnvValue(t, env, "ENVBUILDER_GIT_URL", spec.RepoURL)
	assertEnvValue(t, env, "ENVBUILDER_GIT_REF", spec.Ref)
	assertEnvValue(t, env, "ENVBUILDER_INIT_SCRIPT", "exit 0")
	assertEnvValue(t, env, "ENVBUILDER_PUSH_IMAGE", "true")
	if containsEnvKey(env, "ENVBUILDER_IMAGE_DEST") {
		t.Error("ENVBUILDER_IMAGE_DEST must not be set (it does not exist in envbuilder)")
	}
}

// No Docker socket is ever mounted: kaniko-based envbuilder never talks to
// dockerd, so the build container must never receive the host socket.
func TestBuild_NeverBindsDockerSocket(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	if _, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, bind := range f.lastBinds {
		if strings.HasPrefix(bind, "/var/run/docker.sock") {
			t.Fatal("build container must NEVER bind-mount the Docker socket")
		}
	}
	if !containsEnvKey(f.lastEnv, "ENVBUILDER_PUSH_IMAGE") {
		t.Error("push mode must set ENVBUILDER_PUSH_IMAGE")
	}
}

// TestBuildFromDevcontainerFiles_DeliversTarContext pins the git-free
// generated-build delivery mechanism: a HOST bind mount cannot carry the
// context into a containerized wardynd (its own /tmp is not host-visible to
// the daemon — see BuildFromDevcontainerFiles's doc comment), so the
// generated files MUST be streamed into the created container as a tar via
// CopyToContainer, never a bind. Regression: the old MkdirTemp+bind
// implementation staged an empty workspace in that case and every
// containerized build died with exit 1.
func TestBuildFromDevcontainerFiles_DeliversTarContext(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	files := map[string]string{".devcontainer/devcontainer.json": `{"image":"golang:1.22"}`}
	if _, err := b.BuildFromDevcontainerFiles(t.Context(), files, "wardyn-ws:gen", nil); err != nil {
		t.Fatalf("BuildFromDevcontainerFiles: %v", err)
	}

	if len(f.copiedTo) != 1 || f.copiedTo[0] != "fake-build-container" {
		t.Fatalf("copiedTo = %v, want exactly one CopyToContainer call naming the created container", f.copiedTo)
	}
	if len(f.copiedTars) != 1 || f.copiedTars[0] == 0 {
		t.Fatalf("copiedTars = %v, want exactly one non-empty tar stream", f.copiedTars)
	}
	// The generated context's own delivery adds no extra bind (extraBinds is
	// nil at BuildFromDevcontainerFiles' runBuildAndFinalize call site) — the
	// whole point of the tar path is that nothing here needs a host bind.
	if len(f.lastBinds) != 0 {
		t.Errorf("lastBinds = %v, want none: the generated context must never be bind-mounted from the host", f.lastBinds)
	}
}

// Regression for the CRITICAL host-root finding: registry PUSH is the only
// delivery path, so a build without a CacheRepo must FAIL CLOSED (the retired
// docker.sock fallback no longer exists).
func TestBuild_FailsClosedWithoutCacheRepo(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "") // no CacheRepo
	b.ToolsDir = fakeToolsDir(t)

	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	})
	if err == nil {
		t.Fatal("expected Build to fail closed without a registry (CacheRepo)")
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Errorf("error should steer to setting a registry repo, got: %v", err)
	}
	if f.createCalled != 0 {
		t.Error("must not create a build container when failing closed")
	}
}

// ---------------------------------------------------------------------------
// Build-sandbox hardening: defaults applied to the build container
// ---------------------------------------------------------------------------

// clearSandboxEnv neutralises the env-var fallbacks so default-assertion tests
// are deterministic regardless of the host environment.
func clearSandboxEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WARDYN_ENVBUILD_BUILD_NETWORK", "")
	t.Setenv("WARDYN_ENVBUILD_BUILD_MEMORY_MB", "")
	t.Setenv("WARDYN_ENVBUILD_BUILD_CPUS", "")
	t.Setenv("WARDYN_ENVBUILD_MAX_CONTEXT_MB", "")
}

// By default the untrusted build code must get NO network: NetworkMode "none".
func TestBuild_DefaultsToNoNetwork(t *testing.T) {
	clearSandboxEnv(t)
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	if _, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := string(f.lastNetworkMode); got != "none" {
		t.Errorf("default build NetworkMode = %q, want \"none\"", got)
	}
}

// An explicit BuildNetwork opt-in widens the build container's network.
func TestBuild_NetworkOptInHonored(t *testing.T) {
	clearSandboxEnv(t)
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)
	b.BuildNetwork = "bridge"

	if _, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := string(f.lastNetworkMode); got != "bridge" {
		t.Errorf("opt-in build NetworkMode = %q, want \"bridge\"", got)
	}
}

// Resource caps (memory/swap/cpu/pids) are always applied to the build container.
func TestBuild_AppliesResourceCaps(t *testing.T) {
	clearSandboxEnv(t)
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	if _, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	r := f.lastResources
	if r.Memory != defaultBuildMemoryBytes {
		t.Errorf("Memory = %d, want %d", r.Memory, defaultBuildMemoryBytes)
	}
	if r.MemorySwap != defaultBuildMemoryBytes {
		t.Errorf("MemorySwap = %d, want %d (swap disabled)", r.MemorySwap, defaultBuildMemoryBytes)
	}
	if r.NanoCPUs != defaultBuildNanoCPUs {
		t.Errorf("NanoCPUs = %d, want %d", r.NanoCPUs, defaultBuildNanoCPUs)
	}
	if r.PidsLimit == nil || *r.PidsLimit != defaultBuildPidsLimit {
		t.Errorf("PidsLimit = %v, want %d", r.PidsLimit, defaultBuildPidsLimit)
	}
}

// TestBuild_AppliesExactCapabilitySet pins hardenedHostConfig's capability
// allowlist EXACTLY: CapDrop ALL, then CapAdd back only the file-ownership set
// an image builder cannot extract layers/features without (see the "chown
// /etc/gshadow" regression note on hardenedHostConfig). A wider CapAdd here
// would silently regress the build sandbox's blast-radius bound.
func TestBuild_AppliesExactCapabilitySet(t *testing.T) {
	clearSandboxEnv(t)
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	if _, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://github.com/example/repo",
		OutputImageTag: "wardyn-ws:abc",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantDrop := []string{"ALL"}
	if !slices.Equal(f.lastCapDrop, wantDrop) {
		t.Errorf("CapDrop = %v, want %v", f.lastCapDrop, wantDrop)
	}
	wantAdd := []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID", "SETFCAP", "MKNOD"}
	if !slices.Equal(f.lastCapAdd, wantAdd) {
		t.Errorf("CapAdd = %v, want %v", f.lastCapAdd, wantAdd)
	}
}

// The optional writable-layer size cap is OFF unless MaxBuildContextBytes (or
// its env fallback) is set — and applied as StorageOpt "size" when it is.
func TestBuild_StorageOptContextCap(t *testing.T) {
	clearSandboxEnv(t)

	t.Run("off by default", func(t *testing.T) {
		f := newFakeEnvbuilderDocker()
		b := newPushBuilder(t, f)
		if _, err := b.Build(t.Context(), BuildSpec{
			RepoURL:        "https://github.com/example/repo",
			OutputImageTag: "wardyn-ws:abc",
		}); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if f.lastStorageOpt != nil {
			t.Errorf("StorageOpt must be unset by default, got %v", f.lastStorageOpt)
		}
	})

	t.Run("applied when set", func(t *testing.T) {
		f := newFakeEnvbuilderDocker()
		b := newPushBuilder(t, f)
		t.Setenv("WARDYN_ENVBUILD_MAX_CONTEXT_MB", "8192") // 8 GiB
		if _, err := b.Build(t.Context(), BuildSpec{
			RepoURL:        "https://github.com/example/repo",
			OutputImageTag: "wardyn-ws:abc",
		}); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := f.lastStorageOpt["size"]; got != "8589934592" {
			t.Errorf("StorageOpt[size] = %q, want \"8589934592\"", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Boot-time orphan sweep (SweepOrphanedBuilds)
// ---------------------------------------------------------------------------

// TestSweepOrphanedBuilds_RemovesOnlyUntrackedLabeledContainers pins the
// reaper's two invariants: it scans with a wardyn.envbuild label filter over
// ALL containers (AutoRemove is off, so an exited one leaks too), and it
// removes exactly the ones this process has no live build tracked for —
// never a build the SAME process is actively running.
func TestSweepOrphanedBuilds_RemovesOnlyUntrackedLabeledContainers(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.listItems = []container.Summary{
		{ID: "orphan-1", Labels: map[string]string{envbuildContainerLabel: "wardyn-workspace/aaa:devcontainer"}},
		{ID: "live-1", Labels: map[string]string{envbuildContainerLabel: "wardyn-workspace/bbb:devcontainer"}},
	}
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")
	b.liveBuilds.track("live-1") // stays live: this test's untrack is deliberately never called

	if err := b.SweepOrphanedBuilds(t.Context()); err != nil {
		t.Fatalf("SweepOrphanedBuilds: %v", err)
	}

	if !f.lastListAll {
		t.Error("orphan scan must list ALL containers (All: true) — an exited one leaks too since AutoRemove is off")
	}
	if !f.lastListFilters["label"][envbuildContainerLabel] {
		t.Errorf("orphan scan must filter on label %q, got filters %v", envbuildContainerLabel, f.lastListFilters)
	}
	if !slices.Equal(f.removedIDs, []string{"orphan-1"}) {
		t.Errorf("removedIDs = %v, want exactly [\"orphan-1\"] (live-1 is tracked, must survive)", f.removedIDs)
	}
}

// TestSweepOrphanedBuilds_NoneLabeled is the empty-scan smoke case: no
// containers, no removals, no error.
func TestSweepOrphanedBuilds_NoneLabeled(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")

	if err := b.SweepOrphanedBuilds(t.Context()); err != nil {
		t.Fatalf("SweepOrphanedBuilds: %v", err)
	}
	if len(f.removedIDs) != 0 {
		t.Errorf("removedIDs = %v, want none", f.removedIDs)
	}
}

// TestSweepOrphanedBuilds_YoungOrphanSurvives pins the age gate: an untracked
// labeled container can be a build a DIFFERENT wardynd on the same docker
// daemon just started (or this process's own, moments before
// liveBuilds.track landed — ContainerCreate returns before that deferred
// call runs), so liveBuilds absence alone must not be reaped on. A container
// well inside the grace window (2x the build timeout) must survive even
// though it is untracked here.
func TestSweepOrphanedBuilds_YoungOrphanSurvives(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.listItems = []container.Summary{
		{ID: "young", Labels: map[string]string{envbuildContainerLabel: "wardyn-workspace/aaa:devcontainer"},
			Created: time.Now().Add(-5 * time.Minute).Unix()},
	}
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")

	if err := b.SweepOrphanedBuilds(t.Context()); err != nil {
		t.Fatalf("SweepOrphanedBuilds: %v", err)
	}
	if len(f.removedIDs) != 0 {
		t.Errorf("removedIDs = %v, want none — 5m old is well inside the grace window (2x defaultBuildTimeout=30m=60m)", f.removedIDs)
	}
}

// TestSweepOrphanedBuilds_OldOrphanReaped is the flip side of the age gate:
// past twice the build timeout, no build — this process's or another
// instance's — could still legitimately own the container, so it IS reaped.
func TestSweepOrphanedBuilds_OldOrphanReaped(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.listItems = []container.Summary{
		{ID: "stale", Labels: map[string]string{envbuildContainerLabel: "wardyn-workspace/bbb:devcontainer"},
			Created: time.Now().Add(-61 * time.Minute).Unix()},
	}
	b := newWithClient(f, "envbuilder:test", "registry.example.com/wardyn-cache")

	if err := b.SweepOrphanedBuilds(t.Context()); err != nil {
		t.Fatalf("SweepOrphanedBuilds: %v", err)
	}
	if !slices.Equal(f.removedIDs, []string{"stale"}) {
		t.Errorf("removedIDs = %v, want exactly [\"stale\"] (61m old, past the 60m grace window)", f.removedIDs)
	}
}

// TestBuild_StampsEnvbuildLabel pins the label a real build container carries
// so SweepOrphanedBuilds can find it after a crash/restart.
func TestBuild_StampsEnvbuildLabel(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)

	spec := BuildSpec{RepoURL: "https://github.com/example/repo", OutputImageTag: "wardyn-ws:abc"}
	if _, err := b.Build(t.Context(), spec); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := f.lastLabels[envbuildContainerLabel]; got != spec.OutputImageTag {
		t.Errorf("container label %q = %q, want %q", envbuildContainerLabel, got, spec.OutputImageTag)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// containsEnvKey reports whether any element of env starts with "key=".
func containsEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envValue returns the value for key in an env slice, or "" if not present.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

// assertEnvValue checks that env contains "key=want".
func assertEnvValue(t *testing.T, env []string, key, want string) {
	t.Helper()
	got := envValue(env, key)
	if got != want {
		t.Errorf("env %s = %q, want %q", key, got, want)
	}
}

// TestRequiredTools_CanonicalUnion pins the reconciliation: requiredTools
// is the single source of truth and holds the UNION across the build gate, the
// image --selftest, and ci-run.sh staging. Loosening the slice (dropping a tool)
// fails here, so the reconciliation cannot be silently reverted.
func TestRequiredTools_CanonicalUnion(t *testing.T) {
	want := []string{"agent-run", "agent-run-lib.sh", "wardyn-rec", "wardyn-git-helper"}
	if len(requiredTools) != len(want) {
		t.Fatalf("requiredTools = %v, want %v", requiredTools, want)
	}
	for i, name := range want {
		if requiredTools[i] != name {
			t.Fatalf("requiredTools[%d] = %q, want %q (full: %v)", i, requiredTools[i], name, requiredTools)
		}
	}
}

// TestValidateToolsDir_ConsumesEveryRequiredTool is the drift guard: it proves
// validateToolsDir gates on the WHOLE requiredTools list, not a hardcoded subset.
// For each tool it stages a dir holding every OTHER required tool and asserts the
// preflight fails naming the omitted one. If a future edit made the gate stop
// consuming requiredTools (e.g. re-listing a few names inline), dropping a member
// like wardyn-rec would no longer fail — this test catches that regression.
func TestValidateToolsDir_ConsumesEveryRequiredTool(t *testing.T) {
	for _, missing := range requiredTools {
		t.Run("missing_"+missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range requiredTools {
				if name == missing {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatalf("write tool %q: %v", name, err)
				}
			}
			b := newWithClient(newFakeEnvbuilderDocker(), "envbuilder:test", "registry.example.com/wardyn-cache")
			b.ToolsDir = dir
			if _, err := b.validateToolsDir(); err == nil {
				t.Fatalf("validateToolsDir passed with %q missing; gate does not consume the full requiredTools list", missing)
			} else if !strings.Contains(err.Error(), missing) {
				t.Errorf("error must name the missing tool %q, got: %v", missing, err)
			}
		})
	}
}

// TestBuildFinalizeContext_WiresGitCredentialHelper is the regression guard for
// the defect that made `desktop-envelope` red from the day the job was added and
// never once green: the BYOI wrap COPYed the wardyn-git-helper BINARY onto PATH
// but wired NOTHING to it, so git never called it. Any run whose policy declares
// a `github_token` eligible grant then failed `agent-run --selftest` with "a git
// grant is present but the credential helper is not wired". That is exactly what
// examples/policies/demo.json declares, and it is the desktop tier's own managed
// ceiling — so this broke the entire BYOI lane, not a corner of it.
//
// Nothing covered buildFinalizeContext before this test, which is how a wrap
// that produces an unusable image passed every gate.
func TestBuildFinalizeContext_WiresGitCredentialHelper(t *testing.T) {
	toolsDir := t.TempDir()
	for _, name := range requiredTools {
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rdr, err := buildFinalizeContext("ubuntu:24.04", toolsDir)
	if err != nil {
		t.Fatalf("buildFinalizeContext: %v", err)
	}
	files := map[string]string{}
	modes := map[string]int64{}
	tr := tar.NewReader(rdr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar payload %q: %v", h.Name, err)
		}
		files[h.Name] = string(b)
		modes[h.Name] = h.Mode
	}

	gitcfg, ok := files["gitconfig"]
	if !ok {
		t.Fatalf("no gitconfig in the finalize context; got %v", keysOf(files))
	}
	dockerfile := files["Dockerfile"]
	if !strings.Contains(dockerfile, "COPY gitconfig /etc/gitconfig") {
		t.Errorf("Dockerfile does not install the gitconfig — the file rides along unused:\n%s", dockerfile)
	}
	if !strings.Contains(gitcfg, "credential") || !strings.Contains(gitcfg, "wardyn-git-helper") {
		t.Errorf("gitconfig does not wire wardyn-git-helper as a credential helper:\n%s", gitcfg)
	}

	// The caller-auth gate must survive on a base whose home is NOT /home/agent.
	// agent-run-lib.sh's provision_git_helper_secret always writes
	// ${HOME}/.wardyn/git-helper.secret; ubuntu:24.04 runs as root (HOME=/root).
	// A hardcoded agent home here points --secret-file at a path that never
	// exists, and the helper FALLS OPEN when the file is absent — so the gate
	// would be silently off on every BYOI image, with no error anywhere.
	if strings.Contains(gitcfg, "/home/agent") {
		t.Errorf("gitconfig hardcodes /home/agent; a BYOI base has its own home, so the caller-auth gate would silently fall open:\n%s", gitcfg)
	}
	if !strings.Contains(gitcfg, "--secret-file") {
		t.Errorf("gitconfig drops --secret-file, disabling the per-run caller-auth gate outright:\n%s", gitcfg)
	}
	if !strings.Contains(gitcfg, "$HOME") {
		t.Errorf("gitconfig's --secret-file is not $HOME-relative, so it cannot match provision_git_helper_secret on an arbitrary base:\n%s", gitcfg)
	}

	// A stage that needs a shell cannot wrap a distroless/scratch BYOI base, and
	// it breaks the "FROM + COPY only" property assertWrapSafeBase relies on.
	if strings.Contains(dockerfile, "RUN ") {
		t.Errorf("finalize Dockerfile gained a RUN; the wrap must stay FROM+COPY (a BYOI base may carry no shell):\n%s", dockerfile)
	}
	if got := modes["gitconfig"]; got != 0o644 {
		t.Errorf("gitconfig mode = %#o, want 0644 (root-owned: the sandbox user must not rewrite its own helper path)", got)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
