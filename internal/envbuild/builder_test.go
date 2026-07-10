// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !f.pullCalled {
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
	if f.pullCalled {
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
