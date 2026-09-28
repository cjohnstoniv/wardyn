> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Toolchain, build images and recommended builds

## Toolchain-fidelity environment

Dispatch used to set `GOTMPDIR`/`GOCACHE` and the Maven/Gradle JVM proxy sysprops
(`MAVEN_OPTS`/`GRADLE_OPTS`) on every run, on every image. It no longer does: a
workspace run gets exactly the groups its attached sources' scans detected — the
Go group only when a scan found Go, the JVM group only when it found
Maven/Gradle, the union across every attached source (`buildBaseSandboxEnv`,
`internal/api/runs_dispatch.go`). A run with no workspace attached at all —
ad-hoc, a bare `--image` override, scan, or login runs — keeps the full set:
nothing was scanned and nothing declared, so "unknown" must not silently break
those lanes. A workspace whose OWN base image is registry/custom/BYO is not this
lane: the workspace stays attached (`req.Image` is set from it without leaving
`wsRefs`, `internal/api/runs_create.go`), so `runToolchainNeeds` still narrows to
what that workspace's scan found — only a workspace with no attachment at all, or
one lacking a decodable scan profile, falls back to the full set.

`GOTMPDIR` needs the directory to exist, and unlike `GOCACHE` the go tool refuses
to create it — `go test` compiles and EXECS its test binaries there, and the
sandbox mounts `/tmp` noexec, so the first Go command in an image that never
pre-baked the directory failed with `stat ...: no such file or directory`. Nothing
toolchain-specific is baked into any image for this; two runtime guards create the
directory from the env var alone:

- `agent-run`'s session prep (`make_toolchain_dirs`,
  `deploy/images/common/agent-run-lib.sh`), a no-op when `GOTMPDIR` is unset;
- the attach shell's exec wrapper (`internal/runner/docker/session.go`), which
  runs the same `mkdir -p "$GOTMPDIR"` guard before the prompt renders — session
  prep was measured taking 18s to reach its own mkdir while an attach shell opens
  instantly, so a fast first command would otherwise lose the race.


## Recommended builds on compose

"Recommended — built for this workspace" (a devcontainer build via
`internal/envbuild`; mechanism in [ENVBUILD.md](../ENVBUILD.md)) works out of the box
on the compose stack. Four things ship pre-wired:

- a loopback OCI registry sidecar (`WARDYN_ENVBUILD_PUSHED_REF` defaults to
  `127.0.0.1:5010/wardyn/devcontainers`, the host-side pull ref — Docker exempts
  `127.0.0.1` registries from TLS, and it is not reachable off-host; wardynd
  pushes via the in-network `WARDYN_ENVBUILD_CACHE_REPO`,
  `registry:5000/wardyn/devcontainers`);
- builds default ON (`WARDYN_ENVBUILD` defaults to `true` on compose; the
  bare-binary/host-mode default is still off);
- the build context is delivered as a tar streamed into the build container, not
  a bind mount by path (a path staged in the control plane's own filesystem is
  invisible to the host Docker daemon, and built an empty workspace);
- the build container's capability drop grants exactly the file-ownership set an
  image builder needs instead of dropping everything, which otherwise breaks
  rootfs extraction for any featureful build.

### Every generated image carries the claude-code CLI as standard tooling; nothing bakes codex-cli

Every Wardyn-GENERATED recommended image bakes `claude-code` as a real layer —
unconditionally, the way it carries git or curl, regardless of which integrations
the workspace names. Mechanically: a `.devcontainer/Dockerfile` the emitted
`devcontainer.json` points `build.dockerfile` at, carrying a checksum-verified
native install (architecture-detected, sha256-checked against the release
manifest) that runs as root, before every devcontainer feature, inside the
hardened build container (`genStandardTools` folded into `GenerateDevcontainer`,
`internal/workspacescan/gen.go`; proven with a gated integration test that runs
the built image and checks `claude --version`). codex-cli is not in the standard
set — no Wardyn-verified native-download contract, and its npm lane would need a
Node runtime the bake stage doesn't carry: the image is built without it, never
with a guessed URL. A devcontainer's own `onCreateCommand`/`postCreateCommand`
cannot be used either way: envbuilder runs lifecycle commands AFTER the image is
pushed, so they never reach the delivered image.

**This only applies to Wardyn's OWN generated devcontainer.** When the workspace's
primary source is a repo carrying its own devcontainer file (and it is
HTTPS-cloneable — an SSH source falls through to the generated path, since the
image builder has no SSH-clone wiring), `resolveWorkspaceImage`
(`internal/api/workspace_run.go`) builds that devcontainer AS-IS via
`ImageBuilder.BuildDevcontainer` — cloned and built verbatim, never injected into.
An agent CLI is present there only if the repo's own devcontainer installs it; an
agent run on an image without one fails at the CLI, visibly, rather than being
silently patched.


## Claude sign-in image

The `anthropic_subscription` model-provider kind (each person's own Claude
subscription, captured by a container sign-in) needs a login sandbox that carries
the real `claude` CLI. Wardyn does not publish that image — `agent-claude-code`
bundles a vendor CLI that is not open-source and whose terms are not readable
from the image (`deploy/images/THIRD-PARTY-TERMS.md`) — so every install that
wants to offer the kind builds it locally and the daemon must be told where it
is:

```
make agent-images-core            # builds wardyn/agent-claude-code:local (+ the other core images)
```

`make setup` (both containerized and host mode) already runs this and pins the
result: the compose stack's `WARDYN_AGENT_IMAGES` default and `scripts/run-host.sh`'s
own default both name `wardyn/agent-claude-code:local` for `claude-code`, so a
stock `make setup` needs nothing further. Point at a different image (a
registry mirror, a corp-built tag) with `WARDYN_AGENT_IMAGES='{"claude-code":"<ref>"}'`
(compose: in `deploy/compose/.env`; host mode: exported before `run-host.sh`).

**"Resolves" is two checks, not one.** A pin alone is not proof the image
exists — this repo's own defaults pin it unconditionally, build or no build —
so Wardyn also asks the wired Runner to confirm the pinned ref is actually
present (the docker substrate's local image store; on Kubernetes, which pulls
fresh per launch, the pin is trusted and a missing image surfaces at the login
run itself, naming it). Until both hold, `PUT /model-providers` and
`PUT /site-config` refuse a write that adds an `anthropic_subscription` provider
that is on (E4): *"Claude subscriptions need the Claude Code sign-in image, which
this install hasn't built yet."* Turning a stored subscription back on counts
as adding it. A provider that is off, and one already stored on, are never refused: if the image goes missing later (a prune, a daemon swap),
turning the provider off, editing other providers and re-applying the site
config all keep working.

`GET /api/v1/setup/status` carries this as its own row, `claude_signin_image` —
`info` once it resolves, `warn` with the fix above when it does not. It is
never `blocking`: the kind is optional, so an install offering only API-key or
Bedrock providers is never funneled back into setup over it.


