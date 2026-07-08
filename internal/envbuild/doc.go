// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild converts a devcontainer.json repository into a runnable
// workspace image by driving the coder/envbuilder container as a Docker
// container (not a Go library import). This keeps the envbuilder dependency
// out of the main module's dependency tree while staying within the
// Apache-2.0 license perimeter.
//
// # Envbuilder environment variables used
//
//   - ENVBUILDER_GIT_URL     — repository URL to clone (required)
//   - ENVBUILDER_GIT_REF     — branch/tag/SHA to check out (optional, default: main)
//   - ENVBUILDER_DEVCONTAINER_PATH — path to devcontainer.json inside the repo
//     (optional, default: .devcontainer/devcontainer.json)
//   - ENVBUILDER_PUSH_IMAGE  — set to "true" when a ENVBUILDER_CACHE_REPO is
//     provided; tells envbuilder to push the newly built image to the cache
//     registry (v0.5 feature, local daemon only in v0)
//   - ENVBUILDER_CACHE_REPO  — OCI registry ref used for layer caching
//     (optional; if empty, layer caching is disabled)
//   - ENVBUILDER_IMAGE_DEST  — destination image reference under which
//     envbuilder commits the final image into the local Docker daemon
//     (required; matches BuildSpec.OutputImageTag)
//
// # Build sandbox (UNTRUSTED build code)
//
// Building an image from a devcontainer/Dockerfile executes attacker-controlled
// instructions — Dockerfile RUN steps, devcontainer "features" install scripts,
// and onCreate/updateContent commands — inside the build. For a tool whose whole
// premise is confinement, that build must be treated as untrusted-code execution
// and its blast radius minimised. Builder applies, by default:
//
//   - Image delivery defaults to the daemonless registry PUSH path (CacheRepo)
//     and otherwise FAILS CLOSED. The legacy host-daemon path that bind-mounts
//     /var/run/docker.sock is OFF by default and gated behind
//     Builder.AllowDockerSock (WARDYN_ENVBUILD_ALLOW_DOCKER_SOCK). A writable
//     Docker socket inside a container running untrusted build code is a trivial
//     host-root escape; only enable it for trusted, single-host dev.
//   - Build-time NETWORK defaults to "none" (Builder.BuildNetwork /
//     WARDYN_ENVBUILD_BUILD_NETWORK): the untrusted build code gets no network
//     reachability — no exfiltration, no SSRF to host-local services, no fetching
//     of second-stage payloads — unless an operator explicitly opts in.
//   - Privileges are dropped: CapDrop ALL + no-new-privileges.
//   - Resource caps (memory, swap-disabled, CPU, PID limit) bound the DoS /
//     blast-radius surface; an optional StorageOpt "size" cap
//     (Builder.MaxBuildContextBytes / WARDYN_ENVBUILD_MAX_CONTEXT_MB) bounds the
//     build's writable layer where the storage driver supports per-container
//     quotas.
//   - Input validation: a git-URL scheme allowlist (only https:// and git://;
//     file://, ssh://, scp-like, and ext::/<helper>:: transports are rejected),
//     control-char/leading-dash ref rejection, length bounds on all inputs, and
//     a repo-relative DevcontainerPath check (no absolute path, no ".." traversal).
//
// # Residual (NOT fully closed)
//
// In the envbuilder-as-container model the untrusted RUN/feature/onCreate steps
// still execute inside the build container's namespaces. Two gaps remain:
//
//   - Network: envbuilder needs the network to clone and to pull base images, so
//     a *functional* build requires opting BuildNetwork into a network — at which
//     point the RUN steps share that same network. Isolating RUN-step egress from
//     the clone/pull traffic requires a BuildKit-style builder that applies
//     --network=none to RUN only (future).
//   - The build context is the repo envbuilder clones itself, so it cannot be
//     measured or symlink-resolved on the host before the build; the
//     DevcontainerPath check rejects only syntactic traversal, and the size bound
//     is enforced at runtime via StorageOpt rather than pre-clone.
//
// Fully isolating the build (untrusted RUN steps with no host/daemon trust)
// requires a rootless/microVM builder (e.g. BuildKit rootless, or a microVM such
// as Firecracker/Kata). That is intentionally out of scope here to avoid a hard
// dependency on a new external tool; the controls above minimise the blast radius
// of the residual.
//
// # v0 limitations
//
//   - Images are committed to the local Docker daemon only. There is no
//     registry push path in v0; the built image is available as a local tag
//     suitable for runner.SandboxSpec.Image.
//   - Registry cache (ENVBUILDER_CACHE_REPO / ENVBUILDER_PUSH_IMAGE) is
//     plumbed through as-is but is declared a v0.5 feature: callers may leave
//     CacheRepo empty and no cache behaviour is applied.
//   - Build logs are streamed to the io.Writer supplied in BuildSpec.LogSink.
//     If LogSink is nil, build output is discarded.
//   - The build container itself is always removed on completion or timeout,
//     regardless of success or failure (fail closed on orphaned containers).
package envbuild
