# envbuild — devcontainer builds (two stages)

`internal/envbuild` converts a devcontainer.json repository into a runnable
workspace image by driving the coder/envbuilder container as a Docker container
(not a Go library import), which keeps the envbuilder dependency out of the main
module's dependency tree. This page is the operator-facing detail; the package
comment on `internal/envbuild/builder.go` carries the short version.

## How to trigger a build

The server must be started with an image builder wired (`-tags docker` +
`WARDYN_ENVBUILD`, see [ENV.md](ENV.md)). Then:

```sh
wardyn run --agent claude-code --task "…" \
  --devcontainer-repo org/env-repo --devcontainer-ref main
```

The equivalent API body (POST `/api/v1/runs`):

```json
{"agent":"claude-code","devcontainer_repo":"org/env-repo","devcontainer_ref":"main"}
```

`--devcontainer-repo` is mutually exclusive with `--image` (BYOI), which the
server enforces with a 400. **With NO builder wired the run does not fail — it
silently falls back to the convention image.** Check the `image:` line the CLI
prints on create: a real build is tagged `wardyn-devcontainer/<run-id>:latest`.
`wardyn run --dry-run` checks the same body without launching.

## Two-stage build

A build is two stages:

1. envbuilder (in an untrusted-code sandbox container) clones/reads the repo,
  builds the devcontainer image, and PUSHES it to the configured OCI
  registry (ENVBUILDER_CACHE_REPO + ENVBUILDER_PUSH_IMAGE). kaniko-based
  envbuilder never talks to a Docker daemon; the registry push is the ONLY
  delivery mechanism. After the push, ENVBUILDER_INIT_SCRIPT="exit 0" makes
  the container exit (otherwise envbuilder execs its default init,
  "sleep infinity", and runs forever — the wait-for-exit would hang until
  the build timeout).
2. FINALIZE (a host-daemon build: FROM the pushed image + COPY, no untrusted
  RUN) layers Wardyn's runner tools onto PATH and clears ENTRYPOINT, producing
  the local image tag the runner exec's/verifies/records into. The finalize
  stage COPYs everything in the tools dir, so extra tools (e.g. wardyn-scan) may
  ride along; only these four are contractually required — agent-run,
  agent-run-lib.sh, wardyn-rec, wardyn-git-helper — and a tools
  dir missing any of them fails the build closed (requiredTools /
  validateToolsDir in `internal/envbuild/builder.go`). Without this the built
  image lacks Wardyn's binaries and the runner cannot drive it (H5). Build
  returns this local tag.

The same FINALIZE stage is exposed on its own as FinalizeBase — the
Bring-Your-Own-Image (BYOI) path, which wraps an operator-named base image with
no envbuilder stage at all.

**Concurrent builds get their own push ref.** CacheRepo is one field on a
long-lived Builder, shared by every build the process runs. Stage 1 does not
push to that bare repo (which resolves to the shared `:latest`) — each call to
Build/BuildFromDevcontainerFiles mints a random per-build tag
(`Builder.newPushRef`) and tells envbuilder to push there instead
(`ENVBUILDER_CACHE_REPO=<CacheRepo>:<tag>`), and stage 2 pulls that SAME exact
tag back (`Builder.pushedBaseRef`). Two workspaces building at once therefore
never resolve each other's push: without this, a workspace-B push landing
between workspace A's envbuilder push and finalize pull would get silently
pulled and permanently tagged as workspace A's image (confinement bypass;
finalizeImage's fetch-fresh pull has no re-validation step to catch the swap).
No serialization is needed — each build's destination is unambiguous by
construction — so this preserves full build concurrency.

## Wrap-only (why FINALIZE can run unsandboxed)

FINALIZE runs on the host daemon, outside the untrusted-build sandbox and
outside every confinement tier. That is only safe because it is wrap-ONLY: a
FROM + COPY adds layers and executes nothing the base image controls. Docker's
ONBUILD would break exactly that property — triggers baked into a base fire
when it is used as a FROM, so an `ONBUILD RUN curl … | sh` in a hostile or
compromised base is host-side, build-time RCE. The daemon offers no flag to
suppress triggers and does not report them in the build stream, so Builder
preflights the base with ImageInspect and REFUSES to wrap one that declares any
(assertWrapSafeBase). The base is also pulled by Builder rather than by the
daemon's PullParent, so the wrap builds FROM the exact image the preflight
inspected instead of one the daemon re-resolves afterwards.

## Base-image trust (BYOI)

Wrapping is not vetting. Beyond the ONBUILD refusal, the CONTENT of a BYOI base
is trusted-by-the-operator: Wardyn does not scan it, and a base ref may be a
mutable tag or a digest-pinned ref (repo@sha256:...). Pinning is honored
end-to-end — a pre-pulled digest base matches without a registry round-trip —
and is the recommended operator practice, but it is NOT enforced: a tag is
resolved at wrap time, so what it points at is the operator's call. What the
base's content cannot reach is the host: it only ever executes later, inside the
run's confinement tier. See threatmodel/THREAT-MODEL.md §5 (residual 13).

## Envbuilder environment variables used

- ENVBUILDER_GIT_URL     — repository URL to clone (git path only; omitted
  for the local-context path, which builds from a bind-mounted folder)
- ENVBUILDER_GIT_REF     — branch/tag/SHA to check out (optional, default: main)
- ENVBUILDER_DEVCONTAINER_PATH — path to devcontainer.json inside the repo
  (optional, default: .devcontainer/devcontainer.json)
- ENVBUILDER_WORKSPACE_FOLDER — build-context folder (local-context path:
  the bind-mounted generated files)
- ENVBUILDER_CACHE_REPO  — OCI registry ref to push the built image to
  (required; delivery is via this push). Set to Builder.CacheRepo PLUS a
  random per-build tag (`<CacheRepo>:<uuid>`), never the bare repo — see
  "Concurrent builds get their own push ref" above
- ENVBUILDER_PUSH_IMAGE  — "true"; make envbuilder push the built image to
  ENVBUILDER_CACHE_REPO
- ENVBUILDER_INIT_SCRIPT — "exit 0"; make the post-build exec return so the
  build container exits after the push instead of idling forever
- ENVBUILDER_INSECURE — "true" when WARDYN_ENVBUILD_REGISTRY_INSECURE opts in
  (see "Build sandbox" below); skips TLS verification for EVERY registry
  envbuilder talks to (push, pull, cache probe alike — envbuilder exposes no
  narrower per-registry form). Omitted (envbuilder's own default: verify)
  otherwise.

Note: there is NO ENVBUILDER_IMAGE_DEST — that variable does not exist in any
envbuilder release and was silently ignored. Delivery is the registry push,
not a local-daemon commit.

## Build sandbox (UNTRUSTED build code)

Building an image from a devcontainer/Dockerfile executes attacker-controlled
instructions — Dockerfile RUN steps, devcontainer "features" install scripts,
and onCreate/updateContent commands — inside the build. For a tool whose whole
premise is confinement, that build must be treated as untrusted-code execution
and its blast radius minimised. Builder applies, by default:

- Image delivery is the daemonless registry PUSH path (CacheRepo) and
  otherwise FAILS CLOSED. No Docker socket is ever mounted into the build
  container: kaniko-based envbuilder pushes to the registry and never talks
  to dockerd, so a socket mount would grant a trivial host-root escape for
  zero function.
- Build-time NETWORK defaults to "none" (Builder.BuildNetwork /
  WARDYN_ENVBUILD_BUILD_NETWORK): the untrusted build code gets no network
  reachability — no exfiltration, no SSRF to host-local services, no fetching
  of second-stage payloads — unless an operator explicitly opts in. **Never
  set this to "host"** on a deployment that also publishes anything
  loopback-only for its own convenience (the compose stack's control-plane
  Postgres and admin API, for instance): "host" puts the untrusted build in
  the SAME network namespace as the operator, with the SAME reach to
  whatever is bound to 127.0.0.1 — including default demo credentials
  published in this repo. The compose stack instead defaults this to the
  wardyn-internal bridge (opting in to *that* network only, not the host's),
  and reaches its own registry sidecar by compose service name
  (`registry:5000`) rather than the loopback publish a host-networked build
  container could otherwise use; WARDYN_ENVBUILD_REGISTRY_INSECURE tells
  envbuilder that registry has no TLS to verify, since a non-loopback address
  gets no automatic exemption from that check. A repo's OWN devcontainer
  build still runs on whatever network is configured — this bounds where the
  build container reaches, not what a devcontainer explicitly configured to
  reach it does with that reachability.
- Privileges are dropped: CapDrop ALL + no-new-privileges.
- Resource caps (memory, swap-disabled, CPU, PID limit) bound the DoS /
  blast-radius surface; an optional StorageOpt "size" cap
  (WARDYN_ENVBUILD_MAX_CONTEXT_MB) bounds the build's writable layer where
  the storage driver supports per-container quotas.
- Input validation: a git-URL scheme allowlist (only https:// and git://;
  file://, ssh://, scp-like, and ext::/<helper>:: transports are rejected),
  control-char/leading-dash ref rejection, length bounds on all inputs, and
  a repo-relative DevcontainerPath check (no absolute path, no ".." traversal).

## Residual (NOT fully closed)

In the envbuilder-as-container model the untrusted RUN/feature/onCreate steps
still execute inside the build container's namespaces. Two gaps remain:

- Network: envbuilder needs the network to clone, to pull base images, and to
  PUSH the built image to the cache registry, so a *functional* build requires
  opting BuildNetwork into a network — at which point the RUN steps share that
  same network. Isolating RUN-step egress from the clone/pull/push traffic
  requires a BuildKit-style builder that applies --network=none to RUN only
  (future).
- The build context is the repo envbuilder clones itself, so it cannot be
  measured or symlink-resolved on the host before the build; the
  DevcontainerPath check rejects only syntactic traversal, and the size bound
  is enforced at runtime via StorageOpt rather than pre-clone.

Fully isolating the build (untrusted RUN steps with no host/daemon trust)
requires a rootless/microVM builder (e.g. BuildKit rootless, or a microVM such
as Firecracker/Kata). That is intentionally out of scope here to avoid a hard
dependency on a new external tool; the controls above minimise the blast radius
of the residual.

## Limitations

- A CacheRepo (writable OCI registry repository) is REQUIRED. envbuilder
  pushes the built image there and the finalize stage layers Wardyn's runner
  tools on top; with no registry there is no delivery path and Build fails
  closed. There is no local-daemon-commit path.
- The FINAL image ref envbuilder pushes to is under Wardyn's control (a
  random per-build tag on CacheRepo — see "Concurrent builds get their own
  push ref" above), but envbuilder's own kaniko-style LAYER cache within that
  same repo is content-addressed and outside Wardyn's naming. If a registry
  ever fails to expose the final push at the exact ref Wardyn told envbuilder
  to use, WARDYN_ENVBUILD_PUSHED_REF overrides the REPOSITORY ADDRESS the
  finalize stage pulls from (e.g. a host-loopback path to a registry the build
  container reaches by service name — the compose stack's own use); the
  per-build tag is still appended on top of that override, so it never
  reintroduces one shared ref across builds. See Builder.pushedBaseRef. A
  real-registry TestBuild_SmokeDockerd validates the exact ref.
- Build logs are streamed to the io.Writer supplied in BuildSpec.LogSink.
  If LogSink is nil, build output is discarded.
- The build container itself is always removed on completion or timeout,
  regardless of success or failure (fail closed on orphaned containers).
