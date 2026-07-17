// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild is documented in doc.go.
package envbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
)

const (
	// defaultEnvbuilderImage is the upstream envbuilder release image. Callers
	// may override via Builder.EnvbuilderImage for air-gapped or pinned setups.
	defaultEnvbuilderImage = "ghcr.io/coder/envbuilder:latest"

	// defaultBuildTimeout caps runaway builds so a stuck git-clone or package
	// download does not hold a container slot indefinitely.
	defaultBuildTimeout = 30 * time.Minute

	// defaultBuildNetwork is the Docker network mode applied to the build
	// container when none is configured. "none" denies the untrusted build code
	// (Dockerfile RUN, devcontainer feature installs, onCreate/updateContent
	// commands) any network reachability by default. It must be explicitly
	// widened (see Builder.BuildNetwork) for a real build, because envbuilder
	// itself needs the network to clone and to pull base images.
	defaultBuildNetwork = "none"

	// Default resource caps for the build container. They bound the DoS /
	// blast-radius surface of untrusted build code and are universally
	// supported by the Docker daemon, so they are always applied.
	defaultBuildMemoryBytes = int64(4) << 30        // 4 GiB
	defaultBuildNanoCPUs    = int64(2) * 1000000000 // 2.0 CPUs (1e9 == 1 CPU)
	defaultBuildPidsLimit   = int64(2048)

	// maxBuildInputLen bounds caller-supplied string inputs (URL/ref/path/tag)
	// to defeat pathological or argument-smuggling specs before they reach
	// envbuilder/git.
	maxBuildInputLen = 2048
)

// Environment variables that tune the build sandbox when the corresponding
// Builder field is left at its zero value. They are read inside this package so
// the builder stays self-contained (a future caller may instead set the fields
// directly, which always take precedence over the env fallback).
const (
	envBuildNetwork  = "WARDYN_ENVBUILD_BUILD_NETWORK"
	envBuildMemoryMB = "WARDYN_ENVBUILD_BUILD_MEMORY_MB"
	envBuildCPUs     = "WARDYN_ENVBUILD_BUILD_CPUS"
	envMaxContextMB  = "WARDYN_ENVBUILD_MAX_CONTEXT_MB"

	// envToolsDir points at the host directory holding the runner tool binaries
	// the finalize stage layers onto the built image (see Builder.ToolsDir).
	envToolsDir = "WARDYN_ENVBUILD_TOOLS_DIR"

	// envPushedRef overrides the FROM ref of the finalize stage when envbuilder's
	// pushed image is not resolvable at the plain CacheRepo ref (see pushedBaseRef).
	envPushedRef = "WARDYN_ENVBUILD_PUSHED_REF"
)

// requiredTools are the Wardyn runner binaries that MUST be present in a built
// image for the runner to exec a task, verify, and broker git into it. The
// finalize stage COPYs them (plus anything else in the tools dir, e.g.
// agent-run-lib.sh) from the host ToolsDir; a build fails closed if any is
// missing, because an image without them is unrunnable (H5).
var requiredTools = []string{"agent-run", "wardyn-verify", "wardyn-git-helper"}

// envbuilderDockerAPI is the narrow slice of the Docker client that Builder
// needs. It mirrors the pattern in internal/runner/docker: the interface is
// defined locally (not imported from that package) so the dep graph stays
// clean while the same *client.Client satisfies both interfaces.
type envbuilderDockerAPI interface {
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error)
	// ImageInspect reads a local image's config; the wrap build uses it to read
	// the base's ONBUILD triggers before using it as a FROM (see assertWrapSafeBase).
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)

	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// the real client must implement our slice.
var _ envbuilderDockerAPI = (*client.Client)(nil)

// Builder drives coder/envbuilder as a container to turn a devcontainer.json
// repository into a local workspace image.
type Builder struct {
	// cli is the Docker API client; set by New / newWithClient.
	cli envbuilderDockerAPI

	// EnvbuilderImage is the envbuilder OCI image reference to use.
	// Defaults to defaultEnvbuilderImage.
	EnvbuilderImage string

	// CacheRepo is the OCI registry repository envbuilder pushes the built image
	// to (ENVBUILDER_CACHE_REPO + ENVBUILDER_PUSH_IMAGE=true). Registry PUSH is
	// the ONLY delivery path: kaniko-based envbuilder never talks to dockerd, so
	// there is no local-daemon commit and no Docker socket is ever mounted. A
	// build with an empty CacheRepo fails closed.
	CacheRepo string

	// ToolsDir is the host directory holding Wardyn's runner tool binaries
	// (agent-run, wardyn-verify, wardyn-git-helper — see requiredTools). After
	// envbuilder pushes the base image, the finalize stage COPYs everything in
	// this dir onto the image's PATH so the runner can exec/verify/record into
	// the built image (H5). Empty => WARDYN_ENVBUILD_TOOLS_DIR. A build fails
	// closed if the dir or any required tool is missing.
	ToolsDir string

	// BuildTimeout caps total build time. Zero uses defaultBuildTimeout.
	BuildTimeout time.Duration

	// --- Build-sandbox hardening (the build runs UNTRUSTED, repo-controlled
	// code: Dockerfile RUN, devcontainer feature installs, onCreate /
	// updateContent commands). These knobs bound its blast radius. All have
	// secure defaults; leave them at the zero value to accept the defaults. ---

	// BuildNetwork is the Docker network mode for the build container. Empty =>
	// defaultBuildNetwork ("none" — no network for the untrusted build code),
	// falling back to WARDYN_ENVBUILD_BUILD_NETWORK. Set to "bridge"/"host"/a
	// pre-created named network to OPT IN to build-time egress. Opting in is
	// required for a functional build, because envbuilder must reach git hosts
	// and package/base-image registries; doing so also gives the untrusted RUN
	// steps that same network (see the residual note in doc.go).
	BuildNetwork string
}

// BuildSpec describes one workspace image build.
type BuildSpec struct {
	// RepoURL is the git URL envbuilder will clone (ENVBUILDER_GIT_URL).
	RepoURL string
	// Ref is the git branch/tag/SHA to check out (ENVBUILDER_GIT_REF).
	// Optional; envbuilder default applies when empty.
	Ref string
	// DevcontainerPath is the path inside the repo to devcontainer.json
	// (ENVBUILDER_DEVCONTAINER_PATH). Optional; envbuilder default applies
	// when empty.
	DevcontainerPath string
	// OutputImageTag is the local Docker image reference the FINALIZE stage tags
	// (FROM the pushed base + COPY runner tools). Build returns this tag; it is
	// what callers pass to runner.SandboxSpec.Image after a successful build.
	OutputImageTag string
	// LogSink receives build log bytes streamed from the envbuilder container.
	// If nil, build output is discarded.
	LogSink io.Writer
}

// New constructs a Builder connected to the host Docker daemon with API version
// negotiation. envbuilderImage may be empty to use the default.
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

// Build runs envbuilder for the given spec, then finalizes the pushed base image
// with Wardyn's runner tools, and returns the resolvable local image reference
// (spec.OutputImageTag) on success.
//
// Constraints:
//   - Registry PUSH is the only delivery path: fails closed when CacheRepo is
//     empty (there is no local-daemon fallback).
//   - Fails closed on any non-zero container exit code.
//   - Cancelling ctx or exceeding BuildTimeout kills the build container before
//     returning.
//   - The build container is always force-removed on exit (success, error, or
//     timeout) so no orphaned containers can accumulate.
func (b *Builder) Build(ctx context.Context, spec BuildSpec) (imageRef string, err error) {
	if spec.OutputImageTag == "" {
		return "", fmt.Errorf("envbuild: BuildSpec.OutputImageTag is required")
	}
	// Validate the repo URL/ref BEFORE they reach envbuilder. These are
	// caller-supplied and flow straight into ENVBUILDER_GIT_URL/REF; without a
	// scheme allowlist a file://, ssh://, or `ext::<cmd>` transport helper turns
	// a "clone" into local-file disclosure or arbitrary command execution in the
	// build container (SSRF / RCE). Mirrors the hardened agent-clone path.
	if err := validateBuildInput(spec); err != nil {
		return "", err
	}

	return b.runBuildAndFinalize(ctx, buildEnv(spec, b.CacheRepo), nil, spec.LogSink, spec.OutputImageTag)
}

// runBuildAndFinalize drives the container lifecycle shared by Build and
// BuildFromDevcontainerFiles: registry+tools preflight, timeout, image pull,
// container create/start/wait with env and extraBinds applied atop
// hardenedHostConfig, always-force-remove, optional log streaming, and the
// finalize stage that layers Wardyn's runner tools onto the pushed base image.
func (b *Builder) runBuildAndFinalize(ctx context.Context, env []string, extraBinds []string, logSink io.Writer, outputTag string) (string, error) {
	// Registry PUSH is the only delivery path (the docker.sock fallback is
	// retired). Fail closed with an actionable error when no registry is set.
	if err := b.requireCacheRepo(); err != nil {
		return "", err
	}
	// Preflight the finalize tool sources up front: a build that cannot produce a
	// runnable image must fail fast, not after minutes of building.
	toolsDir, err := b.validateToolsDir()
	if err != nil {
		return "", err
	}

	timeout := b.BuildTimeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Ensure envbuilder image is present; pull if absent. Fail closed if the
	// pull fails — never attempt a build we cannot provision.
	if err := b.ensureImage(ctx, b.EnvbuilderImage); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image: b.EnvbuilderImage,
		Env:   env,
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
	if logSink != nil {
		go b.streamLogs(ctx, containerID, logSink)
	}

	// Wait for the container to exit. v29 folds the old (status, error) channel
	// pair into one ContainerWaitResult carrying both channels.
	wait := b.cli.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		// Timeout or caller cancellation: force-kill the container. The defer
		// above will remove it.
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

	// envbuilder has pushed the base image to the registry. Layer Wardyn's runner
	// tools onto it and return the resolvable local tag (H5). pullParent=true: the
	// base was just pushed to the registry, so the finalize build must pull it.
	return b.finalizeImage(ctx, b.pushedBaseRef(), outputTag, toolsDir, logSink, true)
}

// FinalizeBase is the Bring-Your-Own-Image path: wrap an arbitrary USER-supplied
// base image with Wardyn's runner tools (agent-run, wardyn-rec, wardyn-verify,
// wardyn-git-helper) and a cleared ENTRYPOINT, producing a runnable image the
// runner can exec/record/verify. Unlike Build, there is NO untrusted-code build
// container and NO registry push — just the FROM+COPY finalize stage on the host
// daemon, so it needs neither a cache repo nor the build sandbox. That stage is
// host-side and unsandboxed, so it is wrap-ONLY: assertWrapSafeBase refuses a
// base carrying ONBUILD triggers, which would otherwise execute image-controlled
// code on the host at wrap time. The base is pulled only if absent (ensureImage),
// so a private image pre-pulled on the host works with no registry-auth wiring.
//
// baseRef may be a mutable tag or a digest-pinned ref (repo@sha256:...); Wardyn
// does NOT require pinning — resolving what a tag points at is the operator's
// call, and a tag is resolved at wrap time. Pinning is honored end-to-end (a
// pre-pulled digest ref matches without a registry round-trip) and is the
// recommended operator practice, not an enforced invariant.
//
// Fails closed if the tools dir is unconfigured/incomplete, the base is
// unpullable, or the base carries ONBUILD triggers.
func (b *Builder) FinalizeBase(ctx context.Context, baseRef, outputTag string) (string, error) {
	toolsDir, err := b.validateToolsDir()
	if err != nil {
		return "", err
	}
	if err := b.ensureImage(ctx, baseRef); err != nil {
		return "", fmt.Errorf("envbuild: BYOI base image %q not pullable/present: %w "+
			"(pre-pull a private image on the host with `docker pull`)", baseRef, err)
	}
	// pullParent=false: ensureImage already made the base present locally; a
	// registry pull here would fail for a local-only or digest-pinned user image.
	return b.finalizeImage(ctx, baseRef, outputTag, toolsDir, nil, false)
}

// hardenedHostConfig builds the Docker HostConfig for the build container with
// the untrusted-code blast-radius controls applied: a locked-down network mode
// (default "none"), dropped privileges/capabilities, CPU/memory/PID resource
// caps, and an optional writable-layer size cap. No Docker socket is ever
// mounted — kaniko-based envbuilder pushes to the registry and never talks to
// dockerd, so the build container is never granted host-daemon access.
//
// Returns an error when an env-tunable cap is set to something unparseable: a
// build whose blast-radius controls cannot be resolved as the operator wrote
// them must not run with silently-substituted ones.
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
	pids := b.effectivePidsLimit()
	hostCfg := &container.HostConfig{
		AutoRemove: false, // we remove explicitly via defer to always force-remove.

		// Build-time network. Default "none": untrusted RUN/feature/onCreate code
		// in the build container gets NO network reachability (no exfiltration, no
		// SSRF to host-local services, no fetching of second-stage payloads).
		// Opt in via Builder.BuildNetwork / WARDYN_ENVBUILD_BUILD_NETWORK. NOTE:
		// envbuilder needs the network to clone, to pull base images, and to PUSH
		// the built image to the cache registry, so a functional build requires
		// opting in — at which point the RUN steps share that network. Full
		// RUN-step network isolation needs a BuildKit-style builder (--network=none
		// for RUN only); see doc.go "Residual".
		NetworkMode: container.NetworkMode(b.effectiveBuildNetwork()),

		// Drop privileges so a compromised build step has minimal capability.
		SecurityOpt: []string{"no-new-privileges"},
		CapDrop:     []string{"ALL"},

		// Resource caps bound the DoS / blast-radius surface of untrusted build
		// code. Always applied (universally supported by the daemon).
		Resources: container.Resources{
			Memory:     mem,
			MemorySwap: mem, // == Memory disables swap growth on top of the RAM cap
			NanoCPUs:   cpus,
			PidsLimit:  &pids,
		},
	}

	// Optional disk/context bound on the build container's writable layer. OFF by
	// default: StorageOpt "size" requires a storage driver that supports
	// per-container quotas (e.g. overlay2 on xfs with pquota); enabling it on an
	// unsupported driver makes ContainerCreate fail, so operators must opt in.
	if maxCtx > 0 {
		hostCfg.StorageOpt = map[string]string{"size": strconv.FormatInt(maxCtx, 10)}
	}

	return hostCfg, nil
}

// effectiveBuildNetwork resolves the build-container network mode: the
// Builder.BuildNetwork field, else WARDYN_ENVBUILD_BUILD_NETWORK, else the
// locked-down default ("none").
func (b *Builder) effectiveBuildNetwork() string {
	if v := strings.TrimSpace(b.BuildNetwork); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envBuildNetwork)); v != "" {
		return v
	}
	return defaultBuildNetwork
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

// effectivePidsLimit resolves the build-container process cap.
func (b *Builder) effectivePidsLimit() int64 {
	return defaultBuildPidsLimit
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

// envInt64 parses a non-negative int64 from env key. Unset/empty is 0, meaning
// "not configured" — the caller then applies its own default. A value that is
// present but unparseable or negative is an ERROR, never 0: mapping bad input
// onto "not configured" silently discards an operator-set bound, and for
// envMaxContextMB that specific misread turns the build's writable-layer cap
// OFF. The build fails closed instead, naming the variable and its value.
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

// envFloat parses a non-negative float64 from env key. Same contract as
// envInt64: unset/empty is 0 ("not configured"), bad input is an error.
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

// validateBuildInput enforces a scheme allowlist on the caller-supplied git
// URL/ref before they reach envbuilder. envbuilder passes ENVBUILDER_GIT_URL to
// git, whose transport helpers make several schemes dangerous in a build that
// runs untrusted code: file:// / /local/path (host file disclosure), ext::<cmd>
// (arbitrary command execution), and ssh:// (key/agent abuse). Only https:// and
// plain git:// remote clones are permitted; refs must be sane git ref chars.
func validateBuildInput(spec BuildSpec) error {
	// Bound every caller-supplied string so a pathological spec cannot smuggle a
	// huge / crafted value into envbuilder's environment or git's argv.
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
	// Reject git's `ext::`/`fd::` and any `<transport>::` helper smuggled past
	// the prefix check, plus the scp-like `user@host:path` form which git treats
	// as ssh.
	if strings.Contains(u, "::") {
		return fmt.Errorf("envbuild: RepoURL must not contain a git transport-helper (\"::\") sequence")
	}
	if r := spec.Ref; r != "" {
		if strings.ContainsAny(r, " \t\r\n\x00") || strings.HasPrefix(r, "-") {
			return fmt.Errorf("envbuild: Ref %q contains illegal characters or a leading dash", r)
		}
	}
	// The devcontainer path flows into ENVBUILDER_DEVCONTAINER_PATH and is read
	// relative to the cloned repo root. Constrain it to a repo-relative path with
	// no parent-directory traversal so it cannot point envbuilder at a file
	// outside the cloned tree (e.g. an absolute path or "../../etc/...").
	if p := spec.DevcontainerPath; p != "" {
		if err := validateRepoRelPath("DevcontainerPath", p); err != nil {
			return err
		}
	}
	return nil
}

// validateRepoRelPath ensures a caller-supplied path stays inside the cloned
// repo: it must be relative (no absolute or drive/backslash path), contain no
// parent-directory ("..") traversal, and no NUL/whitespace/leading-dash that
// could escape the repo root or be misread as a git/envbuilder option. Symlink
// resolution happens inside the builder against the cloned tree and therefore
// cannot be checked host-side; see the residual note in doc.go.
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

// buildEnv constructs the envbuilder container environment from a BuildSpec.
// This function is pure (no side effects) so spec->env mapping can be tested
// without a Docker daemon.
//
// Delivery is registry PUSH: ENVBUILDER_CACHE_REPO + ENVBUILDER_PUSH_IMAGE=true
// make kaniko push the built image to the registry (there is no
// ENVBUILDER_IMAGE_DEST — that var does not exist in any envbuilder release).
// ENVBUILDER_INIT_SCRIPT="exit 0" makes envbuilder's post-build exec return
// immediately so the build container EXITS after the push; without it envbuilder
// runs its default init ("sleep infinity") forever and the ContainerWait hangs
// until the build times out.
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

// streamLogs attaches to the build container's log stream and copies to w.
// Errors are silently swallowed; log streaming is best-effort and must not
// affect the build result.
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
	_, _ = io.Copy(w, rc)
}

// ensureImage pulls ref if not already present locally. Fail closed: never
// attempt a build with an absent builder image.
//
// A digest-pinned ref (repo@sha256:...) is stored under RepoDigests, NOT
// RepoTags, so it must be matched against both — otherwise a pre-pulled
// digest-pinned BYOI base would never match, fall through to a pull, and fail
// for a private/local-only image with no registry auth (mirrors the driver's
// imagePresent @sha256: handling, so FinalizeBase honors the same pre-pull
// workflow the docker driver does).
func (b *Builder) ensureImage(ctx context.Context, ref string) error {
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

// requireCacheRepo enforces that a registry repository is configured. Registry
// PUSH is the only delivery path since the docker.sock local-daemon fallback was
// retired, so a build without a CacheRepo cannot deliver an image and must fail
// closed with an actionable error.
func (b *Builder) requireCacheRepo() error {
	if strings.TrimSpace(b.CacheRepo) == "" {
		return errors.New("envbuild: refusing to build: no cache/registry repo configured. " +
			"Set WARDYN_ENVBUILD_CACHE_REPO (or -envbuild-cache-repo) to a writable OCI " +
			"registry repository — envbuilder pushes the built image there and Wardyn " +
			"finalizes it into a runnable local image")
	}
	return nil
}

// validateToolsDir resolves the runner-tools directory (Builder.ToolsDir, else
// WARDYN_ENVBUILD_TOOLS_DIR) and fails closed unless every required tool is
// present. This is the build-contract preflight: an image that lacks
// agent-run / wardyn-verify / wardyn-git-helper cannot be exec'd or verified by
// the runner, so producing one would hand back a broken tag (H5).
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

// pushedBaseRef is the registry reference the finalize stage uses as its FROM
// base: the image envbuilder pushed via ENVBUILDER_PUSH_IMAGE.
//
// ponytail: ASSUMPTION — envbuilder's push is resolvable at the plain CacheRepo
// ref (resolving to :latest when CacheRepo carries no tag). envbuilder actually
// tags the push with a content-addressed cache key (<CacheRepo>@sha256:<digest>)
// that is not knowable host-side before the build, so this is the residual: if
// your registry does not also expose the push at the CacheRepo ref, set
// WARDYN_ENVBUILD_PUSHED_REF to the exact ref (or point CacheRepo at a tagged
// repo). TestBuild_SmokeDockerd against a real registry validates the exact ref.
// Upgrade path: parse the pushed ref from envbuilder's build log / a
// GET_CACHED_IMAGE probe instead of assuming it.
func (b *Builder) pushedBaseRef() string {
	if v := strings.TrimSpace(os.Getenv(envPushedRef)); v != "" {
		return v
	}
	return strings.TrimSpace(b.CacheRepo)
}

// finalizeImage runs the second-stage build (H5): FROM the image envbuilder
// pushed, COPY Wardyn's runner tools onto PATH, and tag the result as the local
// outputTag the runner will use. It runs on the host daemon with only a FROM +
// COPY and no untrusted RUN — assertWrapSafeBase enforces that "no untrusted
// RUN" property against ONBUILD-carrying bases — so it does not need the
// untrusted-code build sandbox. Returns the resolvable local tag on success.
//
// pullBase makes the base pulled fresh from the registry before the wrap
// (devcontainer path: envbuilder just pushed it). BYOI passes false because
// ensureImage already made the possibly local-only/private base present.
func (b *Builder) finalizeImage(ctx context.Context, baseRef, outputTag, toolsDir string, logSink io.Writer, pullBase bool) (string, error) {
	if strings.ContainsAny(baseRef, " \t\r\n\x00") {
		return "", fmt.Errorf("envbuild: finalize base ref %q contains illegal whitespace/control characters", baseRef)
	}
	// Pull here rather than via ImageBuild's PullParent so the ONBUILD preflight
	// below inspects the SAME image the wrap build will resolve as its FROM: a
	// daemon-side PullParent happens after the inspect and could swap a mutable
	// tag underneath it (TOCTOU).
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
		// The base is already local and has passed the ONBUILD preflight (pulled
		// fresh above for the devcontainer path, ensureImage'd for BYOI). Re-pulling
		// here would build from an image the preflight never saw.
		PullParent: false,
	})
	if err != nil {
		return "", fmt.Errorf("envbuild: finalize image build (COPY runner tools): %w", err)
	}
	defer resp.Body.Close()
	// ImageBuild returns nil even when the build fails; the failure (e.g. a bad
	// FROM or a missing COPY source) arrives as an {"error":...} in the response
	// stream, so draining it is how we detect it. This is the second half of the
	// build-contract preflight.
	if err := drainBuildResponse(resp.Body, logSink); err != nil {
		return "", fmt.Errorf("envbuild: finalize image build failed: %w", err)
	}
	return outputTag, nil
}

// assertWrapSafeBase enforces the wrap-only contract: wrapping a base image adds
// layers, it never EXECUTES base-controlled code on the host.
//
// A Docker ONBUILD trigger baked into an image fires when that image is used as a
// FROM — i.e. inside Wardyn's finalize/wrap build, on the HOST daemon, outside
// every confinement tier and outside the untrusted-build sandbox that the
// devcontainer path uses. An `ONBUILD RUN curl … | sh` in a hostile or
// compromised BYOI base is therefore host-side build-time RCE, and it is what
// makes "the wrap is trusted because it is only a FROM + COPY" false. The Docker
// builder has no flag to suppress triggers and does not report them in the build
// stream, so refusing the base is the only fail-closed move.
//
// The direct base is the whole check: ONBUILD fires exactly one level down and a
// child image does not inherit its parent's triggers, so the image we inspect
// carries precisely the triggers that would run.
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

// finalizeDockerfile is the trusted second-stage Dockerfile. It clears
// ENTRYPOINT so the runner's Cmd (sleep infinity / agent-run) runs directly — an
// inherited ENTRYPOINT would wrap it and tear the sandbox down immediately (the
// agent-image contract). The tools land 0755 on PATH via the tar entry mode, so
// no RUN chmod (and no shell in the base image) is required.
func finalizeDockerfile(baseRef string) string {
	return "FROM " + baseRef + "\n" +
		"COPY tools/ /usr/local/bin/\n" +
		"ENTRYPOINT []\n" +
		"CMD []\n"
}

// buildFinalizeContext assembles the in-memory tar build context: the generated
// Dockerfile plus every file in toolsDir staged under tools/ with an executable
// mode so COPY preserves it.
func buildFinalizeContext(baseRef, toolsDir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := writeTarFile(tw, "Dockerfile", []byte(finalizeDockerfile(baseRef)), 0o644); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		return nil, fmt.Errorf("envbuild: read tools dir %q: %w", toolsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // ponytail: flat tools dir; nested dirs aren't part of the contract
		}
		data, err := os.ReadFile(filepath.Join(toolsDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("envbuild: read tool %q: %w", e.Name(), err)
		}
		// 0755 so COPY drops the tools onto PATH already executable (no RUN chmod).
		if err := writeTarFile(tw, "tools/"+e.Name(), data, 0o755); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("envbuild: finalize tar close: %w", err)
	}
	return &buf, nil
}

// writeTarFile writes one regular file into the tar with the given mode.
func writeTarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
	}); err != nil {
		return fmt.Errorf("envbuild: tar header %q: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("envbuild: tar write %q: %w", name, err)
	}
	return nil
}

// drainBuildResponse consumes the daemon's JSON build-output stream, forwarding
// human-readable "stream" text to logSink (if any) and returning the first build
// error reported in the stream.
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
