// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild converts a devcontainer.json repository into a runnable workspace image by
// driving the coder/envbuilder container as a Docker container, keeping envbuilder out of the main
// module's dependency tree. A build has two stages: envbuilder clones the repo and builds the
// devcontainer image in an untrusted-code sandbox, delivering it by pushing to the configured OCI
// registry (fails closed with no registry). Finalize then layers Wardyn's runner tools onto that
// image via a host-daemon FROM+COPY build; FinalizeBase exposes that stage alone as the
// Bring-Your-Own-Image path. Finalize runs outside every confinement tier, so it is wrap-only: bases
// with Docker ONBUILD triggers are refused by preflight (assertWrapSafeBase). See docs/ENVBUILD.md.
package envbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
)

const (
	// defaultEnvbuilderImage is the upstream envbuilder release image, pinned by tag AND digest so the
	// default cannot drift; override via Builder.EnvbuilderImage for air-gapped or newer-pin setups.
	defaultEnvbuilderImage = "ghcr.io/coder/envbuilder:1.3.0@sha256:b34ade2fb90a8536df76e7a15c6dd8c6352d0ae835a187b13467fa0c8a71e280"

	// defaultBuildTimeout caps runaway builds so a stuck git-clone or package download doesn't hold a container slot indefinitely.
	defaultBuildTimeout = 30 * time.Minute

	// defaultBuildNetwork is the network mode when none is configured; "none" denies untrusted build code any reachability.
	defaultBuildNetwork = "none"

	// Default resource caps for the build container; bound the blast radius of untrusted build code and are always applied.
	defaultBuildMemoryBytes = int64(4) << 30        // 4 GiB
	defaultBuildNanoCPUs    = int64(2) * 1000000000 // 2.0 CPUs (1e9 == 1 CPU)
	defaultBuildPidsLimit   = int64(2048)

	// maxBuildInputLen bounds caller-supplied string inputs (URL/ref/path/tag) to defeat argument-smuggling specs before they reach envbuilder/git.
	maxBuildInputLen = 2048
)

// Environment variables that tune the build sandbox; Builder fields take precedence when set.
const (
	envBuildNetwork  = "WARDYN_ENVBUILD_BUILD_NETWORK"
	envBuildMemoryMB = "WARDYN_ENVBUILD_BUILD_MEMORY_MB"
	envBuildCPUs     = "WARDYN_ENVBUILD_BUILD_CPUS"
	envMaxContextMB  = "WARDYN_ENVBUILD_MAX_CONTEXT_MB"

	// envToolsDir points at the host directory holding the runner tool binaries the finalize stage layers onto the built image (see Builder.ToolsDir).
	envToolsDir = "WARDYN_ENVBUILD_TOOLS_DIR"

	// envPushedRef overrides the repository ADDRESS the finalize stage pulls the pushed base from,
	// when the host daemon reaches the registry differently than the build container (see
	// docs/ENVBUILD.md); it names an address only — the per-build tag is always appended on top.
	envPushedRef = "WARDYN_ENVBUILD_PUSHED_REF"

	// envRegistryInsecure opts envbuilder's registry traffic out of TLS verification
	// (ENVBUILDER_INSECURE), a blanket toggle not scoped to one host; off by default.
	envRegistryInsecure = "WARDYN_ENVBUILD_REGISTRY_INSECURE"
)

// requiredTools is the single canonical declaration of the Wardyn runner tools that must be present
// in a built/wrapped image for the runner to exec, record, verify, and broker git into it; every gate
// in this package consumes this list (the shell sites agent-run --selftest/ci-run.sh keep their own hardcoded checks).
var requiredTools = []string{
	"agent-run",         // task entrypoint the runner execs
	"agent-run-lib.sh",  // sourced by agent-run under `set -euo pipefail` (load-bearing)
	"wardyn-rec",        // session recorder; required by the image --selftest
	"wardyn-git-helper", // brokered-token git credential helper
}

// Builder drives coder/envbuilder as a container to turn a devcontainer.json repository into a local workspace image.
type Builder struct {
	// cli is the Docker API client; set by New / newWithClient.
	cli        envbuilderDockerAPI
	liveBuilds liveBuildTracker // in-flight build container IDs; see reaper.go

	// EnvbuilderImage is the envbuilder OCI image reference to use. Defaults to defaultEnvbuilderImage.
	EnvbuilderImage string

	// CacheRepo is the registry repo envbuilder pushes to; the only delivery path (no Docker socket mounted). Empty fails closed.
	CacheRepo string

	// ToolsDir is the host dir holding Wardyn's runner tools (requiredTools); finalize COPYs them onto
	// the image PATH. Empty => WARDYN_ENVBUILD_TOOLS_DIR.
	ToolsDir string

	// DefaultLogSink receives build output when a call passes no per-call sink.
	DefaultLogSink io.Writer

	// BuildTimeout caps total build time. Zero uses defaultBuildTimeout.
	BuildTimeout time.Duration

	// --- Build-sandbox hardening: these knobs bound the blast radius of untrusted repo-controlled build code and default to secure values. ---

	// BuildNetwork is the network mode for the build container. Empty => defaultBuildNetwork ("none")
	// or WARDYN_ENVBUILD_BUILD_NETWORK; opting in also gives the untrusted RUN steps that same network access.
	BuildNetwork string
}

// BuildSpec describes one workspace image build.
type BuildSpec struct {
	// RepoURL is the git URL envbuilder will clone (ENVBUILDER_GIT_URL).
	RepoURL string
	// Ref is the git branch/tag/SHA to check out (ENVBUILDER_GIT_REF); optional, envbuilder default applies when empty.
	Ref string
	// DevcontainerPath is the path inside the repo to devcontainer.json (ENVBUILDER_DEVCONTAINER_PATH). Optional.
	DevcontainerPath string
	// OutputImageTag is the local Docker image reference the finalize stage tags; Build returns this tag for runner.SandboxSpec.Image.
	OutputImageTag string
	// LogSink receives build log bytes; falls back to Builder.DefaultLogSink, else discarded.
	LogSink io.Writer
}

// New constructs a Builder connected to the host Docker daemon with API version
// negotiation; envbuilderImage may be empty to use the default.
func New(envbuilderImage, cacheRepo string) (*Builder, error) {
	cli, err := client.New(
		client.FromEnv,
	)
	if err != nil {
		return nil, fmt.Errorf("envbuild: new docker client: %w", err)
	}
	return newWithClient(cli, envbuilderImage, cacheRepo), nil
}

// newWithClient is the seam used by tests to inject a fake envbuilderDockerAPI.
func newWithClient(cli envbuilderDockerAPI, envbuilderImage, cacheRepo string) *Builder {
	img := envbuilderImage
	if img == "" {
		img = defaultEnvbuilderImage
	}
	return &Builder{
		cli:             cli,
		EnvbuilderImage: img,
		CacheRepo:       cacheRepo,
	}
}

// Build runs envbuilder for the given spec, then finalizes the pushed base image with Wardyn's
// runner tools, returning spec.OutputImageTag on success. Fails closed with no CacheRepo or a
// non-zero exit code; the build container is always force-removed on exit, including on cancellation/timeout.
func (b *Builder) Build(ctx context.Context, spec BuildSpec) (imageRef string, err error) {
	if spec.OutputImageTag == "" {
		return "", fmt.Errorf("envbuild: BuildSpec.OutputImageTag is required")
	}
	// Without a scheme allowlist, file://, ssh://, or ext::<cmd> transports
	// enable host file disclosure or RCE in the build container.
	if err := validateBuildInput(spec); err != nil {
		return "", err
	}

	pushRepo, buildTag := b.newPushRef()
	return b.runBuildAndFinalize(ctx, buildEnv(spec, pushRepo), nil, nil, "", spec.LogSink, spec.OutputImageTag, buildTag)
}

// runBuildAndFinalize drives the container lifecycle shared by Build and BuildFromDevcontainerFiles:
// preflight, timeout, create/start/wait, always-force-remove, optional log streaming, and finalize.
// contextTar, when non-nil, is streamed into the container at contextTarDest before start (a bind
// mount cannot carry it). buildTag is this build's per-build push tag, pulled back via pushedBaseRef
// so concurrent builds on one CacheRepo never collide.
func (b *Builder) runBuildAndFinalize(ctx context.Context, env []string, extraBinds []string, contextTar io.Reader, contextTarDest string, logSink io.Writer, outputTag, buildTag string) (string, error) {
	// Registry PUSH is the only delivery path; fail closed with no registry set.
	if err := b.requireCacheRepo(); err != nil {
		return "", err
	}
	// Preflight the finalize tool sources so a doomed build fails fast, not after minutes of building.
	toolsDir, err := b.validateToolsDir()
	if err != nil {
		return "", err
	}
	insecure, err := b.effectiveRegistryInsecure()
	if err != nil {
		return "", err
	}
	if insecure {
		env = append(env, "ENVBUILDER_INSECURE=true")
	}

	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Ensure envbuilder image is present; fail closed if the pull fails.
	if err := b.ensureImage(ctx, b.EnvbuilderImage); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:  b.EnvbuilderImage,
		Env:    env,
		Labels: map[string]string{envbuildContainerLabel: outputTag}, // reaper.go's orphan scan
		// envbuilder is the image entrypoint; Cmd is left nil intentionally.
	}
	hostCfg, err := b.hardenedHostConfig()
	if err != nil {
		return "", err
	}
	hostCfg.Binds = append(hostCfg.Binds, extraBinds...)

	created, err := b.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return "", fmt.Errorf("envbuild: create build container: %w", err)
	}
	containerID := created.ID
	defer b.liveBuilds.track(containerID)() // see reaper.go
	// A daemon that accepted the create but discarded a requested limit would run this untrusted build uncapped.
	// Builds always run with their caps, so they take no WARDYN_ALLOW_UNENFORCEABLE_CAPS override.
	if lims := dockerutil.DiscardedLimits(created.Warnings); len(lims) > 0 {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_, _ = b.cli.ContainerRemove(rmCtx, containerID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("environment build refused: the Docker daemon discarded %s; builds always run with their caps: %w",
			strings.Join(lims, "; "), dockerutil.ErrCapsDiscarded)
	}
	if contextTar != nil {
		if _, err := b.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
			DestinationPath: contextTarDest, Content: contextTar,
		}); err != nil {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rmCancel()
			_, _ = b.cli.ContainerRemove(rmCtx, containerID, client.ContainerRemoveOptions{Force: true})
			return "", fmt.Errorf("envbuild: stage build context: %w", err)
		}
	}

	// Always force-remove the build container even on cancellation or panic.
	defer func() {
		// Use a fresh background context: the parent ctx may already be done.
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_, _ = b.cli.ContainerRemove(rmCtx, containerID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := b.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("envbuild: start build container: %w", err)
	}

	// Stream build logs concurrently while waiting for the container to finish.
	if logSink == nil {
		logSink = b.DefaultLogSink
	}
	var streamDone chan struct{}
	if logSink != nil {
		streamDone = make(chan struct{})
		go func() {
			defer close(streamDone)
			b.streamLogs(ctx, containerID, logSink)
		}()
		// Every return path must join this goroutine before returning: a caller reusing DefaultLogSink
		// for the next build would otherwise race this build's still-writing streamLogs.
		defer func() { <-streamDone }()
	}

	// Wait for the container to exit; v29 folds status+error into one ContainerWaitResult.
	wait := b.cli.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		// Timeout or cancellation: the defer above removes the container.
		return "", fmt.Errorf("envbuild: build cancelled or timed out: %w", ctx.Err())

	case waitErr := <-wait.Error:
		return "", fmt.Errorf("envbuild: waiting for build container: %w", waitErr)

	case resp := <-wait.Result:
		if resp.Error != nil {
			return "", fmt.Errorf("envbuild: build container error: %s", resp.Error.Message)
		}
		if resp.StatusCode != 0 {
			return "", fmt.Errorf("envbuild: build failed with exit code %d", resp.StatusCode)
		}
	}

	// Join before finalize writes the same sink below (else the two race); ContainerLogs closes on its
	// own once the container stops, so this can't hang.
	if streamDone != nil {
		<-streamDone
	}

	// envbuilder has pushed the base to its per-build ref; layer Wardyn's runner tools onto it.
	// pullParent=true: the base was just pushed, so finalize must pull it fresh.
	baseRef := b.pushedBaseRef(buildTag)
	defer b.untagPerBuildBase(ctx, baseRef)
	return b.finalizeImage(ctx, baseRef, outputTag, toolsDir, logSink, true)
}

// untagPerBuildBase drops the local tag finalize pulled its FROM base under (docker rmi only untags;
// shared layers and the output tag stay intact), else every build leaves a tag pinning a base image
// against prune. Best-effort, run on the failed path too, and detached from ctx with its own short bound.
func (b *Builder) untagPerBuildBase(ctx context.Context, baseRef string) {
	if strings.TrimSpace(baseRef) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), perBuildBaseUntagTimeout)
	defer cancel()
	if _, err := b.cli.ImageRemove(ctx, baseRef, client.ImageRemoveOptions{}); err != nil {
		slog.Debug("envbuild: could not untag the per-build base image (left for the next prune)",
			slog.String("ref", baseRef), slog.String("error", err.Error()))
	}
}

// perBuildBaseUntagTimeout bounds the best-effort untag above: long enough for a daemon round-trip, short enough that a wedged daemon cannot hold a build.
const perBuildBaseUntagTimeout = 30 * time.Second

// FinalizeBase is the Bring-Your-Own-Image path: wrap an arbitrary user-supplied base image with
// Wardyn's runner tools, with no untrusted-code build container and no registry push — just a
// host-side FROM+COPY, so it is wrap-only (assertWrapSafeBase refuses ONBUILD triggers). The base is
// pulled only if absent; baseRef may be a mutable tag or digest-pinned (pinning honored, not enforced).
func (b *Builder) FinalizeBase(ctx context.Context, baseRef, outputTag string, logSink io.Writer) (string, error) {
	toolsDir, err := b.validateToolsDir()
	if err != nil {
		return "", err
	}
	// Same timeout ceiling as runBuildAndFinalize, needed since a caller may detach from request cancellation.
	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := b.ensureImage(ctx, baseRef); err != nil {
		return "", fmt.Errorf("envbuild: BYOI base image %q not pullable/present: %w "+
			"(pre-pull a private image on the host with `docker pull`)", baseRef, err)
	}
	if logSink == nil {
		logSink = b.DefaultLogSink
	}
	// pullParent=false: ensureImage already made the base present locally; a registry pull here would fail for a local-only or digest-pinned user image.
	return b.finalizeImage(ctx, baseRef, outputTag, toolsDir, logSink, false)
}

// hardenedHostConfig builds the Docker HostConfig for the build container: locked-down network
// (default "none"), dropped privileges/capabilities, resource caps, and an optional writable-layer
// size cap. No Docker socket is ever mounted.
//
// Returns an error when an env-tunable cap is unparseable, rather than silently substituting a default.
func (b *Builder) hardenedHostConfig() (*container.HostConfig, error) {
	mem, err := b.effectiveMemoryBytes()
	if err != nil {
		return nil, err
	}
	cpus, err := b.effectiveNanoCPUs()
	if err != nil {
		return nil, err
	}
	maxCtx, err := b.effectiveMaxContextBytes()
	if err != nil {
		return nil, err
	}
	pids := defaultBuildPidsLimit // no env override, unlike its siblings
	hostCfg := &container.HostConfig{
		AutoRemove: false, // we remove explicitly via defer to always force-remove.

		// Default "none" denies untrusted RUN/feature code any reachability; opting in (Builder.BuildNetwork) also gives RUN steps that network.
		NetworkMode: container.NetworkMode(b.effectiveBuildNetwork()),

		// Drop all privileges, then add back only the file-ownership caps envbuilder/kaniko needs to
		// unpack base layers and apply devcontainer features (chown/chmod/setuid bits); no-new-privileges blocks setuid escalation.
		SecurityOpt: []string{"no-new-privileges"},
		CapDrop:     []string{"ALL"},
		CapAdd: []string{
			"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID",
			"SETGID", "SETUID", "SETFCAP", "MKNOD",
		},

		// Resource caps bound the DoS/blast-radius surface of untrusted build code. Always requested; the create response is checked for a discarded limit (a swap-only "limited without swap" warning is not).
		Resources: container.Resources{
			Memory:     mem,
			MemorySwap: mem, // == Memory disables swap growth on top of the RAM cap
			NanoCPUs:   cpus,
			PidsLimit:  &pids,
		},
	}

	// Optional disk/context bound on the writable layer. Off by default: StorageOpt "size" needs a quota-capable storage driver, so operators opt in.
	if maxCtx > 0 {
		hostCfg.StorageOpt = map[string]string{"size": strconv.FormatInt(maxCtx, 10)}
	}

	return hostCfg, nil
}

// effectiveBuildNetwork resolves the build-container network mode: the Builder.BuildNetwork field, else WARDYN_ENVBUILD_BUILD_NETWORK, else "none".
func (b *Builder) effectiveBuildNetwork() string {
	if v := strings.TrimSpace(b.BuildNetwork); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envBuildNetwork)); v != "" {
		return v
	}
	return defaultBuildNetwork
}

// effectiveRegistryInsecure resolves whether envbuilder should skip TLS verification
// (ENVBUILDER_INSECURE). Unset/empty is false; unparseable is an error, never a silent false.
func (b *Builder) effectiveRegistryInsecure() (bool, error) {
	v := strings.TrimSpace(os.Getenv(envRegistryInsecure))
	if v == "" {
		return false, nil
	}
	insecure, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("envbuild: %s=%q is not a valid bool", envRegistryInsecure, v)
	}
	return insecure, nil
}

// effectiveMemoryBytes resolves the build-container memory cap.
func (b *Builder) effectiveMemoryBytes() (int64, error) {
	mb, err := envInt64(envBuildMemoryMB)
	if err != nil {
		return 0, err
	}
	if mb > 0 {
		return mb << 20, nil
	}
	return defaultBuildMemoryBytes, nil
}

// effectiveNanoCPUs resolves the build-container CPU cap (1e9 == 1 CPU).
func (b *Builder) effectiveNanoCPUs() (int64, error) {
	c, err := envFloat(envBuildCPUs)
	if err != nil {
		return 0, err
	}
	if c > 0 {
		return int64(c * 1e9), nil
	}
	return defaultBuildNanoCPUs, nil
}

// effectiveMaxContextBytes resolves the optional writable-layer size cap. Zero
// (the default) means no StorageOpt size limit is applied.
func (b *Builder) effectiveMaxContextBytes() (int64, error) {
	mb, err := envInt64(envMaxContextMB)
	if err != nil {
		return 0, err
	}
	if mb > 0 {
		return mb << 20, nil
	}
	return 0, nil
}

// envInt64 parses a non-negative int64 from env key. Unset/empty is 0 ("not configured"); a present but unparseable/negative value is an error, never 0.
func envInt64(key string) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("envbuild: invalid %s=%q: want a non-negative integer", key, v)
	}
	return n, nil
}

// envFloat parses a non-negative float64 from env key. Same contract as envInt64: unset/empty is 0, bad input is an error.
func envFloat(key string) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("envbuild: invalid %s=%q: want a non-negative number", key, v)
	}
	return f, nil
}

// validateBuildInput enforces a scheme allowlist on the caller-supplied git URL/ref: file://,
// ext::<cmd>, and ssh:// transports enable host file disclosure, RCE, or key abuse. Only https://
// and git:// clones are permitted; refs must be sane git ref chars.
func validateBuildInput(spec BuildSpec) error {
	// Bound every caller-supplied string so a pathological spec cannot smuggle a huge/crafted value into envbuilder's environment or git's argv.
	for _, f := range []struct{ name, val string }{
		{"RepoURL", spec.RepoURL},
		{"Ref", spec.Ref},
		{"DevcontainerPath", spec.DevcontainerPath},
		{"OutputImageTag", spec.OutputImageTag},
	} {
		if len(f.val) > maxBuildInputLen {
			return fmt.Errorf("envbuild: %s exceeds the %d-byte input bound", f.name, maxBuildInputLen)
		}
	}

	// The output tag reaches envbuilder's env and the finalize Dockerfile, so it must pass the same validateGeneratedTag check as the generated-files path.
	if err := validateGeneratedTag(spec.OutputImageTag); err != nil {
		return err
	}

	u := spec.RepoURL
	if u == "" {
		return fmt.Errorf("envbuild: BuildSpec.RepoURL is required")
	}
	if strings.ContainsAny(u, " \t\r\n\x00") {
		return fmt.Errorf("envbuild: RepoURL contains illegal whitespace/control characters")
	}
	lower := strings.ToLower(u)
	allowed := strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "git://")
	if !allowed {
		return fmt.Errorf("envbuild: RepoURL scheme not allowed (%q); only https:// and git:// "+
			"remote clones are permitted (file://, ssh://, and ext:: transports are rejected)", u)
	}
	// Reject `ext::`/`fd::`/any `<transport>::` helper smuggled past the prefix check, plus the scp-like `user@host:path` form git treats as ssh.
	if strings.Contains(u, "::") {
		return fmt.Errorf("envbuild: RepoURL must not contain a git transport-helper (\"::\") sequence")
	}
	if r := spec.Ref; r != "" {
		if strings.ContainsAny(r, " \t\r\n\x00") || strings.HasPrefix(r, "-") {
			return fmt.Errorf("envbuild: Ref %q contains illegal characters or a leading dash", r)
		}
	}
	// The devcontainer path is read relative to the cloned repo root; constrain it to repo-relative with no ".." traversal so it cannot escape the tree.
	if p := spec.DevcontainerPath; p != "" {
		if err := validateRepoRelPath("DevcontainerPath", p); err != nil {
			return err
		}
	}
	return nil
}

// validateRepoRelPath ensures a caller-supplied path stays inside the cloned repo: relative, no ".."
// traversal, no NUL/whitespace/leading-dash. Symlink resolution can't be checked host-side; see docs/ENVBUILD.md.
func validateRepoRelPath(field, p string) error {
	if strings.ContainsAny(p, " \t\r\n\x00") {
		return fmt.Errorf("envbuild: %s %q contains illegal whitespace/control characters", field, p)
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("envbuild: %s %q must not start with '-'", field, p)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("envbuild: %s %q must use forward slashes (no backslash paths)", field, p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("envbuild: %s %q must be relative to the repo root (no absolute path)", field, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("envbuild: %s %q must not contain a '..' path segment (traversal)", field, p)
		}
	}
	if clean := path.Clean(p); clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("envbuild: %s %q escapes the repo root (path traversal)", field, p)
	}
	return nil
}

// buildEnv constructs the envbuilder container environment from a BuildSpec (pure, so testable
// without a daemon). ENVBUILDER_INIT_SCRIPT="exit 0" makes the container exit after the push; without
// it envbuilder's default init ("sleep infinity") runs forever and ContainerWait hangs until timeout.
func buildEnv(spec BuildSpec, cacheRepo string) []string {
	env := []string{
		"ENVBUILDER_GIT_URL=" + spec.RepoURL,
		"ENVBUILDER_INIT_SCRIPT=exit 0",
	}
	if spec.Ref != "" {
		env = append(env, "ENVBUILDER_GIT_REF="+spec.Ref)
	}
	if spec.DevcontainerPath != "" {
		env = append(env, "ENVBUILDER_DEVCONTAINER_PATH="+spec.DevcontainerPath)
	}
	if cacheRepo != "" {
		env = append(env, "ENVBUILDER_CACHE_REPO="+cacheRepo)
		env = append(env, "ENVBUILDER_PUSH_IMAGE=true")
	}
	return env
}

// streamLogs attaches to the build container's log stream and copies to w. Best-effort: errors are
// silently swallowed. No TTY means ContainerLogs multiplexes stdout/stderr behind an 8-byte frame
// header; stdcopy strips it.
func (b *Builder) streamLogs(ctx context.Context, containerID string, w io.Writer) {
	rc, err := b.cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return
	}
	defer rc.Close()
	_, _ = stdcopy.StdCopy(w, w, rc)
}

// ensureImage pulls ref if not already present locally; fails closed rather than building with an
// absent image. A digest-pinned ref is resolved by ImageInspect, not a list scan: the daemon's
// `repo:tag@sha256:…` spelling appears under neither RepoTags nor RepoDigests verbatim.
func (b *Builder) ensureImage(ctx context.Context, ref string) error {
	if strings.Contains(ref, "@sha256:") {
		_, err := b.cli.ImageInspect(ctx, ref)
		if err == nil {
			return nil
		}
		if !errdefs.IsNotFound(err) {
			return fmt.Errorf("envbuild: image inspect %q: %w", ref, err)
		}
		return dockerutil.PullImage(ctx, b.cli, ref, "envbuild")
	}
	res, err := b.cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("envbuild: list images: %w", err)
	}
	for _, s := range res.Items {
		for _, tag := range s.RepoTags {
			if tag == ref {
				return nil
			}
		}
		for _, dig := range s.RepoDigests {
			if dig == ref {
				return nil
			}
		}
	}
	return dockerutil.PullImage(ctx, b.cli, ref, "envbuild")
}

// requireCacheRepo enforces that a registry repository is configured; without one a build cannot deliver an image and must fail closed.
func (b *Builder) requireCacheRepo() error {
	if strings.TrimSpace(b.CacheRepo) == "" {
		return errors.New("envbuild: refusing to build: no cache/registry repo configured. " +
			"Set WARDYN_ENVBUILD_CACHE_REPO (or -envbuild-cache-repo) to a writable OCI " +
			"registry repository — envbuilder pushes the built image there and Wardyn " +
			"finalizes it into a runnable local image")
	}
	return nil
}

// validateToolsDir resolves the runner-tools directory (Builder.ToolsDir, else WARDYN_ENVBUILD_TOOLS_DIR)
// and fails closed unless every required tool is present — the build-contract preflight.
func (b *Builder) validateToolsDir() (string, error) {
	dir := strings.TrimSpace(b.ToolsDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv(envToolsDir))
	}
	if dir == "" {
		return "", fmt.Errorf("envbuild: no runner-tools dir configured: set WARDYN_ENVBUILD_TOOLS_DIR "+
			"to a directory containing %s so the built image is runnable by the runner",
			strings.Join(requiredTools, ", "))
	}
	for _, name := range requiredTools {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("envbuild: required runner tool %q missing from tools dir %q: %w "+
				"(a built image without it cannot be exec'd/verified by the runner)", name, dir, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("envbuild: required runner tool %q in %q is a directory, not a file", name, dir)
		}
	}
	return dir, nil
}

// newPushRef returns a fresh per-build registry ref for this build's envbuilder push, plus the bare
// tag alone. Each build mints its own tag so concurrent builds sharing one CacheRepo never collide on
// a shared :latest. Empty CacheRepo returns ("", ""); requireCacheRepo fails the build closed first.
func (b *Builder) newPushRef() (pushRepo, tag string) {
	repo := strings.TrimSpace(b.CacheRepo)
	if repo == "" {
		return "", ""
	}
	tag = uuid.New().String()
	return repo + ":" + tag, tag
}

// pushedBaseRef is the registry reference the finalize stage pulls FROM: this build's own per-build
// tag, resolved against whichever repository the host daemon reaches that registry at.
// WARDYN_ENVBUILD_PUSHED_REF overrides only the repository address; buildTag is always appended on top.
func (b *Builder) pushedBaseRef(buildTag string) string {
	repo := strings.TrimSpace(os.Getenv(envPushedRef))
	if repo == "" {
		repo = strings.TrimSpace(b.CacheRepo)
	}
	if repo == "" || buildTag == "" {
		return repo
	}
	return repo + ":" + buildTag
}

// finalizeImage runs the second-stage build: FROM the pushed image, COPY Wardyn's runner tools onto
// PATH, tag as outputTag. Only FROM+COPY on the host daemon — no untrusted RUN — so it needs no build
// sandbox. pullBase pulls fresh before the wrap; BYOI passes false since ensureImage already made the base present.
func (b *Builder) finalizeImage(ctx context.Context, baseRef, outputTag, toolsDir string, logSink io.Writer, pullBase bool) (string, error) {
	if strings.ContainsAny(baseRef, " \t\r\n\x00") {
		return "", fmt.Errorf("envbuild: finalize base ref %q contains illegal whitespace/control characters", baseRef)
	}
	// Pull here (not via ImageBuild's PullParent) so the ONBUILD preflight below inspects the exact image the wrap build resolves as FROM (avoids TOCTOU).
	if pullBase {
		if err := dockerutil.PullImage(ctx, b.cli, baseRef, "envbuild"); err != nil {
			return "", err
		}
	}
	if err := b.assertWrapSafeBase(ctx, baseRef); err != nil {
		return "", err
	}
	tarCtx, err := buildFinalizeContext(baseRef, toolsDir)
	if err != nil {
		return "", err
	}
	resp, err := b.cli.ImageBuild(ctx, tarCtx, client.ImageBuildOptions{
		Tags:        []string{outputTag},
		Dockerfile:  "Dockerfile",
		Remove:      true,
		ForceRemove: true,
		// The base already passed the ONBUILD preflight above; re-pulling here would build from an image the preflight never saw.
		PullParent: false,
	})
	if err != nil {
		return "", fmt.Errorf("envbuild: finalize image build (COPY runner tools): %w", err)
	}
	defer resp.Body.Close()
	// ImageBuild returns nil even on failure; the error arrives as {"error":...} in the response stream, so draining it is how failure is detected.
	if err := drainBuildResponse(resp.Body, logSink); err != nil {
		return "", fmt.Errorf("envbuild: finalize image build failed: %w", err)
	}
	return outputTag, nil
}

// assertWrapSafeBase enforces the wrap-only contract: wrapping adds layers, it never executes
// base-controlled code on the host. A Docker ONBUILD trigger fires host-side when the image is used
// as a FROM, so a hostile BYOI base with `ONBUILD RUN curl … | sh` is build-time RCE; Docker cannot
// suppress or report triggers, so refusing the base is the only fail-closed move. Checking only the
// direct base suffices: ONBUILD fires one level down and children don't inherit it.
func (b *Builder) assertWrapSafeBase(ctx context.Context, baseRef string) error {
	res, err := b.cli.ImageInspect(ctx, baseRef)
	if err != nil {
		return fmt.Errorf("envbuild: inspect base image %q before wrapping: %w", baseRef, err)
	}
	if res.Config == nil || len(res.Config.OnBuild) == 0 {
		return nil
	}
	return fmt.Errorf("envbuild: refusing to wrap base image %q: it declares %d ONBUILD trigger(s) "+
		"(first: %q). ONBUILD instructions execute on the HOST Docker daemon during the wrap "+
		"build — outside every confinement tier — so wrapping such a base would run "+
		"image-controlled code on the host. Resolve the triggers in an image you build "+
		"yourself (a Dockerfile with `FROM %s` fires them at YOUR build time) and pass that "+
		"image instead", baseRef, len(res.Config.OnBuild), res.Config.OnBuild[0], baseRef)
}

// drainBuildResponse consumes the daemon's JSON build-output stream, forwarding human-readable
// "stream" text to logSink (if any) and returning the first build error reported in the stream.
func drainBuildResponse(body io.Reader, logSink io.Writer) error {
	dec := json.NewDecoder(body)
	for {
		var msg struct {
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode build stream: %w", err)
		}
		if logSink != nil && msg.Stream != "" {
			_, _ = io.WriteString(logSink, msg.Stream)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
	}
}
