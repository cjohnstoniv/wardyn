// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

// Package envbuild converts a devcontainer.json repository into a runnable
// workspace image by driving the coder/envbuilder container as a Docker
// container (not a Go library import). This keeps the envbuilder dependency
// out of the main module's dependency tree while staying within the
// Apache-2.0 license perimeter.
//
// # Two-stage build
//
// A build is two stages:
//
//  1. envbuilder (in an untrusted-code sandbox container) clones/reads the repo,
//     builds the devcontainer image, and PUSHES it to the configured OCI
//     registry (ENVBUILDER_CACHE_REPO + ENVBUILDER_PUSH_IMAGE). kaniko-based
//     envbuilder never talks to a Docker daemon; the registry push is the ONLY
//     delivery mechanism. After the push, ENVBUILDER_INIT_SCRIPT="exit 0" makes
//     the container exit (otherwise envbuilder execs its default init,
//     "sleep infinity", and runs forever — the wait-for-exit would hang until
//     the build timeout).
//  2. FINALIZE (a trusted host-daemon build: FROM the pushed image + COPY, no
//     untrusted RUN) layers Wardyn's runner tool binaries (agent-run,
//     wardyn-verify, wardyn-git-helper, plus anything else in the tools dir)
//     onto PATH and clears ENTRYPOINT, producing the local image tag the runner
//     exec's/verifies/records into. Without this the built image lacks Wardyn's
//     binaries and the runner cannot drive it (H5). Build returns this local tag.
//
// # Envbuilder environment variables used
//
//   - ENVBUILDER_GIT_URL     — repository URL to clone (git path only; omitted
//     for the local-context path, which builds from a bind-mounted folder)
//   - ENVBUILDER_GIT_REF     — branch/tag/SHA to check out (optional, default: main)
//   - ENVBUILDER_DEVCONTAINER_PATH — path to devcontainer.json inside the repo
//     (optional, default: .devcontainer/devcontainer.json)
//   - ENVBUILDER_WORKSPACE_FOLDER — build-context folder (local-context path:
//     the bind-mounted generated files)
//   - ENVBUILDER_CACHE_REPO  — OCI registry repository to push the built image to
//     (required; delivery is via this push)
//   - ENVBUILDER_PUSH_IMAGE  — "true"; make envbuilder push the built image to
//     ENVBUILDER_CACHE_REPO
//   - ENVBUILDER_INIT_SCRIPT — "exit 0"; make the post-build exec return so the
//     build container exits after the push instead of idling forever
//
// Note: there is NO ENVBUILDER_IMAGE_DEST — that variable does not exist in any
// envbuilder release and was silently ignored. Delivery is the registry push,
// not a local-daemon commit.
//
// # Build sandbox (UNTRUSTED build code)
//
// Building an image from a devcontainer/Dockerfile executes attacker-controlled
// instructions — Dockerfile RUN steps, devcontainer "features" install scripts,
// and onCreate/updateContent commands — inside the build. For a tool whose whole
// premise is confinement, that build must be treated as untrusted-code execution
// and its blast radius minimised. Builder applies, by default:
//
//   - Image delivery is the daemonless registry PUSH path (CacheRepo) and
//     otherwise FAILS CLOSED. No Docker socket is ever mounted into the build
//     container: kaniko-based envbuilder pushes to the registry and never talks
//     to dockerd, so a socket mount would grant a trivial host-root escape for
//     zero function.
//   - Build-time NETWORK defaults to "none" (Builder.BuildNetwork /
//     WARDYN_ENVBUILD_BUILD_NETWORK): the untrusted build code gets no network
//     reachability — no exfiltration, no SSRF to host-local services, no fetching
//     of second-stage payloads — unless an operator explicitly opts in.
//   - Privileges are dropped: CapDrop ALL + no-new-privileges.
//   - Resource caps (memory, swap-disabled, CPU, PID limit) bound the DoS /
//     blast-radius surface; an optional StorageOpt "size" cap
//     (WARDYN_ENVBUILD_MAX_CONTEXT_MB) bounds the build's writable layer where
//     the storage driver supports per-container quotas.
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
//   - Network: envbuilder needs the network to clone, to pull base images, and to
//     PUSH the built image to the cache registry, so a *functional* build requires
//     opting BuildNetwork into a network — at which point the RUN steps share that
//     same network. Isolating RUN-step egress from the clone/pull/push traffic
//     requires a BuildKit-style builder that applies --network=none to RUN only
//     (future).
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
// # Limitations
//
//   - A CacheRepo (writable OCI registry repository) is REQUIRED. envbuilder
//     pushes the built image there and the finalize stage layers Wardyn's runner
//     tools on top; with no registry there is no delivery path and Build fails
//     closed. There is no local-daemon-commit path.
//   - The exact tag envbuilder pushes is content-addressed and not known
//     host-side before the build; the finalize stage assumes it is resolvable at
//     the plain CacheRepo ref and exposes WARDYN_ENVBUILD_PUSHED_REF to override.
//     See Builder.pushedBaseRef. A real-registry TestBuild_SmokeDockerd validates
//     the exact ref.
//   - Build logs are streamed to the io.Writer supplied in BuildSpec.LogSink.
//     If LogSink is nil, build output is discarded.
//   - The build container itself is always removed on completion or timeout,
//     regardless of success or failure (fail closed on orphaned containers).
package envbuild
