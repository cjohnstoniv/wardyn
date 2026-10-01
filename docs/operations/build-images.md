> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Toolchain, build images and recommended builds

How a run's toolchain env vars get set, how "Recommended" devcontainer builds
work on compose, and how to build the Claude sign-in image this install
needs for `anthropic_subscription`.

## Toolchain-fidelity environment

Dispatch used to set `GOTMPDIR`/`GOCACHE` and the Maven/Gradle JVM proxy
sysprops (`MAVEN_OPTS`/`GRADLE_OPTS`) on every run, on every image. It no
longer does.

| Case | What the run gets |
| --- | --- |
| A workspace with attached sources | Exactly the groups its scans detected — the Go group only if a scan found Go, the JVM group only if it found Maven/Gradle. The union applies across every attached source (`buildBaseSandboxEnv`, `internal/api/runs_dispatch.go`) |
| No workspace attached (ad-hoc, `--image` override, scan, or login runs) | The full set. Nothing was scanned and nothing declared, so "unknown" must not silently break those lanes |
| A workspace whose own base image is registry/custom/BYO | Not this lane: the workspace stays attached (`req.Image` is set from it without leaving `wsRefs`, `internal/api/runs_create.go`), so `runToolchainNeeds` still narrows to what that workspace's scan found |

Only a workspace with no attachment at all, or one lacking a decodable scan
profile, falls back to the full set.

- `GOTMPDIR` needs the directory to exist, and unlike `GOCACHE` the go tool
  refuses to create it: `go test` compiles and EXECs its test binaries
  there, and the sandbox mounts `/tmp` noexec.
- The first Go command in an image that never pre-baked the directory used
  to fail with `stat ...: no such file or directory`.
- Nothing toolchain-specific is baked into any image for this. Two runtime
  guards create the directory from the env var alone instead:

1. `agent-run`'s session prep (`make_toolchain_dirs`,
   `deploy/images/common/agent-run-lib.sh`) — a no-op when `GOTMPDIR` is
   unset.
2. The attach shell's exec wrapper (`internal/runner/docker/session.go`),
   which runs the same `mkdir -p "$GOTMPDIR"` guard before the prompt
   renders. Session prep was measured taking 18s to reach its own mkdir,
   while an attach shell opens instantly — a fast first command would
   otherwise lose the race.

## Recommended builds on compose

"Recommended — built for this workspace" (a devcontainer build via
`internal/envbuild`; mechanism in [ENVBUILD.md](../ENVBUILD.md)) works out of
the box on the compose stack. Four things ship pre-wired:

- A loopback OCI registry sidecar. `WARDYN_ENVBUILD_PUSHED_REF` defaults to
  `127.0.0.1:5010/wardyn/devcontainers`, the host-side pull ref — Docker
  exempts `127.0.0.1` registries from TLS, and it isn't reachable off-host.
  wardynd pushes via the in-network `WARDYN_ENVBUILD_CACHE_REPO`,
  `registry:5000/wardyn/devcontainers`.
- Builds default ON. `WARDYN_ENVBUILD` defaults to `true` on compose; the
  bare-binary/host-mode default is still off.
- The build context is delivered as a tar streamed into the build container,
  not a bind mount by path. A path staged in the control plane's own
  filesystem is invisible to the host Docker daemon, and built an empty
  workspace.
- The build container's capability drop grants exactly the file-ownership
  set an image builder needs, instead of dropping everything — which
  otherwise breaks rootfs extraction for any featureful build.

### Every generated image carries claude-code; nothing bakes codex-cli

- Every Wardyn-GENERATED recommended image bakes `claude-code` as a real
  layer, unconditionally — the way it carries git or curl — regardless of
  which integrations the workspace names.
- **Mechanically:** a `.devcontainer/Dockerfile` the emitted
  `devcontainer.json` points `build.dockerfile` at carries a
  checksum-verified native install (architecture-detected, sha256-checked
  against the release manifest). It runs as root, before every devcontainer
  feature, inside the hardened build container (`genStandardTools` folded
  into `GenerateDevcontainer`, `internal/workspacescan/gen.go`). A gated
  integration test runs the built image and checks `claude --version`.
- **codex-cli is not in the standard set.** There's no Wardyn-verified
  native-download contract, and its npm lane would need a Node runtime the
  bake stage doesn't carry. The image is built without it, never with a
  guessed URL.
- **The recipe is deterministic; what it installs is not locked.** The base
  image tag (`mcr.microsoft.com/devcontainers/base:ubuntu`), the `:1` feature
  tags and the claude-code `/stable` channel all resolve at build time. The
  installer checks the binary against that release's own manifest, from the same
  origin as the binary: integrity, not a pin.
- **What the cache key identifies.** `CacheKey = SHA256(ProfileHash|v2)`
  identifies the recipe, not the resolved tools.
  - Reuse needs all three conditions, checked just before a generated image is
    built (`internal/api/workspace_run_image.go`): a stored image ref, a matching
    `BuiltProfileHash`, and `cachedImageStillPresent`. That presence check fails
    open: a runner that cannot answer counts the image as present.
  - A rebuild happens on a profile change, a `cacheKeySalt` bump or a missing
    image. A rebuild **may** resolve newer versions; envbuilder's layer cache can
    replay layers, so it is not a security refresh.
  - A kept image gets no upstream security fixes until it is rebuilt, and there is
    no refresh operation until #1516.
- A devcontainer's own `onCreateCommand`/`postCreateCommand` can't be used
  either way — envbuilder runs lifecycle commands AFTER the image is
  pushed, so they never reach the delivered image.
- **This only applies to Wardyn's OWN generated devcontainer.** When the
  workspace's primary source is a repo carrying its own devcontainer file,
  and it's HTTPS-cloneable, `resolveWorkspaceImage`
  (`internal/api/workspace_run.go`) builds that devcontainer AS-IS via
  `ImageBuilder.BuildDevcontainer` — cloned and built verbatim, never
  injected into. (An SSH source falls through to the generated path
  instead, since the image builder has no SSH-clone wiring.)
- An agent CLI is present there only if the repo's own devcontainer
  installs it. A run on an image without one fails at the CLI, visibly,
  rather than being silently patched.

## Claude sign-in image

The `anthropic_subscription` model-provider kind (each person's own Claude
subscription, captured by a container sign-in) needs a login sandbox that
carries the real `claude` CLI.

Wardyn does not publish that image: `agent-claude-code` bundles a vendor CLI
that is not open-source, and its terms aren't readable from the image
(`deploy/images/THIRD-PARTY-TERMS.md`). Every install that wants to offer
the kind builds it locally.

1. Build the image and tell the daemon where it is:
   ```
   make agent-images-core   # builds wardyn/agent-claude-code:local (+ the other core images)
   ```
2. `make setup` (both containerized and host mode) already runs this and
   pins the result: the compose stack's `WARDYN_AGENT_IMAGES` default and
   `scripts/run-host.sh`'s own default both name
   `wardyn/agent-claude-code:local` for `claude-code`, so a stock
   `make setup` needs nothing further.
3. To point at a different image (a registry mirror, a corp-built tag), set
   `WARDYN_AGENT_IMAGES='{"claude-code":"<ref>"}'` (compose: in
   `deploy/compose/.env`; host mode: exported before `run-host.sh`).

**"Resolves" is two checks, not one:**

- A pin alone is not proof the image exists — this repo's own defaults pin
  it unconditionally, build or no build.
- Wardyn also asks the wired Runner to confirm the pinned ref is actually
  present: the docker substrate's local image store. On Kubernetes, which
  pulls fresh per launch, the pin is trusted and a missing image surfaces
  at the login run itself, naming it.

Until both checks hold, `PUT /model-providers` and `PUT /site-config` refuse
a write that turns an `anthropic_subscription` provider on (E4). The refusal
reads: *"Claude subscriptions need the Claude Code sign-in image, which this
install hasn't built yet."* Turning a stored subscription back on counts as
adding it.

| State | Refused? |
| --- | --- |
| Adding or re-enabling `anthropic_subscription` without the image | Yes (E4) |
| A provider that is off | No |
| A provider already stored on, even if the image goes missing later (a prune, a daemon swap) | No — turning it off, editing other providers and re-applying the site config all keep working |

`GET /api/v1/setup/status` carries this as its own row,
`claude_signin_image` — `info` once it resolves, `warn` with the fix above
when it doesn't. It's never `blocking`: the kind is optional, so an install
offering only API-key or Bedrock providers is never funneled back into setup
over it.
