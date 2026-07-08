// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild is documented in doc.go.
package envbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
)

// envbuilderDockerAPI is the narrow slice of the Docker client that Builder
// needs. It mirrors the pattern in internal/runner/docker: the interface is
// defined locally (not imported from that package) so the dep graph stays
// clean while the same *client.Client satisfies both interfaces.
type envbuilderDockerAPI interface {
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)

	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
}

// the real client must implement our slice.
var _ envbuilderDockerAPI = (*dockerclient.Client)(nil)

// Builder drives coder/envbuilder as a container to turn a devcontainer.json
// repository into a local workspace image.
type Builder struct {
	// cli is the Docker API client; set by New / newWithClient.
	cli envbuilderDockerAPI

	// EnvbuilderImage is the envbuilder OCI image reference to use.
	// Defaults to defaultEnvbuilderImage.
	EnvbuilderImage string

	// CacheRepo is the optional OCI registry ref used by envbuilder for layer
	// caching (ENVBUILDER_CACHE_REPO + ENVBUILDER_PUSH_IMAGE). When set, the
	// build runs in registry PUSH mode and the host Docker socket is NOT mounted
	// — this is the safe, daemonless path.
	CacheRepo string

	// AllowDockerSock opts in to the legacy single-host path that bind-mounts
	// /var/run/docker.sock into the build container so envbuilder can commit the
	// image into the local daemon.
	//
	// SECURITY: this is DANGEROUS and OFF by default. envbuilder executes
	// repo-controlled build code (Dockerfile RUN, devcontainer feature install
	// scripts, onCreate/updateContent commands); a writable Docker socket in that
	// container is a trivial host-root escape (e.g. `docker run -v /:/host`).
	// Only enable it for trusted, single-host dev (compose/kind) where you
	// already trust the repo. Wired from WARDYN_ENVBUILD_ALLOW_DOCKER_SOCK.
	// Prefer CacheRepo (registry push mode) instead.
	AllowDockerSock bool

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

	// BuildMemoryBytes caps build-container memory in bytes. Zero => default
	// (WARDYN_ENVBUILD_BUILD_MEMORY_MB, else defaultBuildMemoryBytes).
	BuildMemoryBytes int64

	// BuildNanoCPUs caps build-container CPU (1e9 == 1 CPU). Zero => default
	// (WARDYN_ENVBUILD_BUILD_CPUS, else defaultBuildNanoCPUs).
	BuildNanoCPUs int64

	// BuildPidsLimit caps the number of processes in the build container.
	// Zero => defaultBuildPidsLimit.
	BuildPidsLimit int64

	// MaxBuildContextBytes, when > 0, caps the build container's writable-layer
	// size via the Docker StorageOpt "size" option. OFF by default because
	// StorageOpt "size" requires a storage driver that supports per-container
	// quotas (e.g. overlay2 on xfs with the pquota mount option); enabling it on
	// an unsupported driver makes ContainerCreate fail. Falls back to
	// WARDYN_ENVBUILD_MAX_CONTEXT_MB.
	MaxBuildContextBytes int64
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
	// OutputImageTag is the local Docker image reference that the built image
	// will be tagged as (ENVBUILDER_IMAGE_DEST). This is what callers pass to
	// runner.SandboxSpec.Image after a successful build.
	OutputImageTag string
	// LogSink receives build log bytes streamed from the envbuilder container.
	// If nil, build output is discarded.
	LogSink io.Writer
}

// New constructs a Builder connected to the host Docker daemon with API version
// negotiation. envbuilderImage may be empty to use the default.
func New(envbuilderImage, cacheRepo string) (*Builder, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
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

// Build runs envbuilder for the given spec and returns the local image reference
// (== spec.OutputImageTag) on success.
//
// Constraints:
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

	// Decide the image-delivery mechanism. Default to the safe, daemonless
	// registry PUSH path; only fall back to the dangerous docker.sock mount when
	// explicitly opted in; otherwise FAIL CLOSED rather than silently exposing
	// the host daemon to untrusted build code.
	pushMode := b.CacheRepo != ""
	if !pushMode && !b.AllowDockerSock {
		return "", fmt.Errorf("envbuild: refusing to build: no CacheRepo configured " +
			"(registry push mode) and the docker.sock fallback is disabled. Set a " +
			"cache/registry repo (recommended) or, for trusted single-host dev only, " +
			"WARDYN_ENVBUILD_ALLOW_DOCKER_SOCK=1")
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

	env := buildEnv(spec, b.CacheRepo)

	cfg := &container.Config{
		Image: b.EnvbuilderImage,
		Env:   env,
		// envbuilder is the image entrypoint; Cmd is left nil intentionally.
	}
	hostCfg := b.hardenedHostConfig(pushMode)

	created, err := b.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("envbuild: create build container: %w", err)
	}
	containerID := created.ID

	// Always force-remove the build container even on cancellation or panic.
	defer func() {
		// Use a fresh background context: the parent ctx may already be done.
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = b.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
	}()

	if err := b.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("envbuild: start build container: %w", err)
	}

	// Stream build logs concurrently while waiting for the container to finish.
	if spec.LogSink != nil {
		go b.streamLogs(ctx, containerID, spec.LogSink)
	}

	// Wait for the container to exit.
	waitCh, errCh := b.cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		// Timeout or caller cancellation: force-kill the container. The defer
		// above will remove it.
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

	return spec.OutputImageTag, nil
}

// hardenedHostConfig builds the Docker HostConfig for the build container with
// the untrusted-code blast-radius controls applied: a locked-down network mode
// (default "none"), dropped privileges/capabilities, CPU/memory/PID resource
// caps, and an optional writable-layer size cap. pushMode selects the safe
// daemonless delivery (no socket) vs. the opt-in docker.sock mount.
func (b *Builder) hardenedHostConfig(pushMode bool) *container.HostConfig {
	mem := b.effectiveMemoryBytes()
	pids := b.effectivePidsLimit()
	hostCfg := &container.HostConfig{
		AutoRemove: false, // we remove explicitly via defer to always force-remove.

		// Build-time network. Default "none": untrusted RUN/feature/onCreate code
		// in the build container gets NO network reachability (no exfiltration, no
		// SSRF to host-local services, no fetching of second-stage payloads).
		// Opt in via Builder.BuildNetwork / WARDYN_ENVBUILD_BUILD_NETWORK. NOTE:
		// envbuilder needs the network to clone and to pull base images, so a
		// functional build requires opting in — at which point the RUN steps share
		// that network. Full RUN-step network isolation needs a BuildKit-style
		// builder (--network=none for RUN only); see doc.go "Residual".
		NetworkMode: container.NetworkMode(b.effectiveBuildNetwork()),

		// Drop privileges so a compromised build step has minimal capability,
		// even on the opt-in socket path. (The socket itself is still a
		// root-equivalent capability; CacheRepo push mode avoids it entirely.)
		SecurityOpt: []string{"no-new-privileges"},
		CapDrop:     []string{"ALL"},

		// Resource caps bound the DoS / blast-radius surface of untrusted build
		// code. Always applied (universally supported by the daemon).
		Resources: container.Resources{
			Memory:     mem,
			MemorySwap: mem, // == Memory disables swap growth on top of the RAM cap
			NanoCPUs:   b.effectiveNanoCPUs(),
			PidsLimit:  &pids,
		},
	}

	// Optional disk/context bound on the build container's writable layer. OFF by
	// default: StorageOpt "size" requires a storage driver that supports
	// per-container quotas (e.g. overlay2 on xfs with pquota); enabling it on an
	// unsupported driver makes ContainerCreate fail, so operators must opt in.
	if maxCtx := b.effectiveMaxContextBytes(); maxCtx > 0 {
		hostCfg.StorageOpt = map[string]string{"size": strconv.FormatInt(maxCtx, 10)}
	}

	if !pushMode {
		// Opt-in, reduced-isolation single-host path: mount the Docker socket so
		// envbuilder can commit the image into the local daemon. DANGEROUS — see
		// Builder.AllowDockerSock. Only reached when AllowDockerSock is true.
		// (Push mode needs no socket; buildEnv already set ENVBUILDER_PUSH_IMAGE
		// from CacheRepo.)
		hostCfg.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	}
	return hostCfg
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
func (b *Builder) effectiveMemoryBytes() int64 {
	if b.BuildMemoryBytes > 0 {
		return b.BuildMemoryBytes
	}
	if mb := envInt64(envBuildMemoryMB); mb > 0 {
		return mb << 20
	}
	return defaultBuildMemoryBytes
}

// effectiveNanoCPUs resolves the build-container CPU cap (1e9 == 1 CPU).
func (b *Builder) effectiveNanoCPUs() int64 {
	if b.BuildNanoCPUs > 0 {
		return b.BuildNanoCPUs
	}
	if c := envFloat(envBuildCPUs); c > 0 {
		return int64(c * 1e9)
	}
	return defaultBuildNanoCPUs
}

// effectivePidsLimit resolves the build-container process cap.
func (b *Builder) effectivePidsLimit() int64 {
	if b.BuildPidsLimit > 0 {
		return b.BuildPidsLimit
	}
	return defaultBuildPidsLimit
}

// effectiveMaxContextBytes resolves the optional writable-layer size cap. Zero
// (the default) means no StorageOpt size limit is applied.
func (b *Builder) effectiveMaxContextBytes() int64 {
	if b.MaxBuildContextBytes > 0 {
		return b.MaxBuildContextBytes
	}
	if mb := envInt64(envMaxContextMB); mb > 0 {
		return mb << 20
	}
	return 0
}

// envInt64 parses a non-negative int64 from env key, or 0 if unset/invalid.
func envInt64(key string) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// envFloat parses a non-negative float64 from env key, or 0 if unset/invalid.
func envFloat(key string) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
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
func buildEnv(spec BuildSpec, cacheRepo string) []string {
	env := []string{
		"ENVBUILDER_GIT_URL=" + spec.RepoURL,
		"ENVBUILDER_IMAGE_DEST=" + spec.OutputImageTag,
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
	rc, err := b.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
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
func (b *Builder) ensureImage(ctx context.Context, ref string) error {
	summaries, err := b.cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return fmt.Errorf("envbuild: list images: %w", err)
	}
	for _, s := range summaries {
		for _, tag := range s.RepoTags {
			if tag == ref {
				return nil
			}
		}
	}
	return dockerutil.PullImage(ctx, b.cli, ref, "envbuild")
}
