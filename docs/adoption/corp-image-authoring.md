<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Authoring Wardyn Dockerfiles behind a corporate proxy / mirror

For anyone EDITING a Dockerfile in this repo on a corporate network. Operator-
facing image docs (the image contract, which image to pick, bring-your-own-image)
stay in [deploy/images/README.md](../../deploy/images/README.md).

## Corporate-build convention (TLS-MITM proxy / internal mirror)

Behind a corporate egress proxy that re-signs TLS with an internal CA and serves
packages only through an internal mirror, an ordinary build fails at the first
`go mod download` / `npm install` with `x509: certificate signed by unknown
authority` (or a registry 404/403). Every Wardyn Dockerfile that fetches
dependencies supports the same opt-in, **no-op-on-OSS** knobs so a corp user can
build with `make setup` / `make agent-images` and **no hand-editing**. When you
add or edit a Dockerfile, mirror these — do not invent a variant.

**1. Corp CA (host-staged, gitignored).** Drop your corporate root/intermediate CA
(PEM) at `deploy/images/corp-ca.pem`. It is gitignored (never commit it). Each
build stage that needs TLS trust stages it into the system trust store:

```dockerfile
# The README.md companion makes the COPY succeed when the corp-ca.pem* glob
# matches nothing (Docker COPY requires ≥1 match — README.md always matches).
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi
```

`update-ca-certificates` needs the package installed first on **Alpine**
(`RUN apk add --no-cache ca-certificates && if [ -f ... ]; then ...; fi`) **and on
`-slim` Debian bases** — `node:*-bookworm-slim` `apt-get purge --auto-remove`s
`ca-certificates` in its own build, so the binary is absent and the snippet above
exits 127. Check before you copy it: `docker run --rm --entrypoint sh <base> -c
'command -v update-ca-certificates'`. On a **distroless** final stage there is no
shell — copy the already-updated bundle:
`COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt`.

For **Node** stages, prefer `ENV NODE_EXTRA_CA_CERTS=/corp-ca/corp-ca.pem` on its
own and skip the system-trust `RUN` entirely: npm/pnpm read that var, and it needs
no package. That is what `deploy/compose/Dockerfile.wardynd`'s `ui-build` does.

**This is the BUILD-TIME, contributor path** — `deploy/images/corp-ca.pem` trusts
a corporate proxy only while these Dockerfiles themselves build, on the machine
running `make setup` / `make agent-images`. It is unrelated to (and does not
substitute for) `WARDYN_TRUSTED_CA_FILE`: the OPERATOR-facing, runtime knob a
published `wardynd` reads at boot to trust a corporate TLS-inspecting middlebox
for the deployment's own traffic — wardynd's outbound TLS, the proxy sidecar,
and every sandbox — with no image rebuild at all. See
[docs/OPERATIONS.md § "Corporate TLS-inspection root"](../OPERATIONS.md#corporate-tls-inspection-root).

**2. Go builds:** add `ENV GOTOOLCHAIN=local` so the pinned-toolchain self-upgrade
fetch (blocked behind a MITM proxy) is skipped; the corp CA above lets
`go mod download` verify the module proxy. Also declare a module-mirror knob
right after it — in the **builder stage** (the one you `COPY --from`), never a
runtime stage, for the same reason as the build-only ARGs in 3 below:

```dockerfile
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
```

Empty is identical to unset — the go command falls back to `$GOROOT/go.env`
(`https://proxy.golang.org,direct`) — so this is a no-op on an OSS build.

**3. npm/pnpm builds:** declare these **build-only ARGs** (never persistent ENV —
a runtime proxy ENV would leak into agent runs, whose only egress is wardyn-proxy)
and apply them before the install:

```dockerfile
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi \
 && if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi \
 && npm install -g <pkg>
```

**4. Wire the knobs to the build entrypoints.** The Makefile threads
`NPM_REGISTRY`/`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`GOPROXY` into every
`docker build` via `$(DOCKER_BUILD_ARGS)`; the compose stanzas pass them through
`build.args`. So a corp user runs, e.g.:

```
make agent-images NPM_REGISTRY=https://mirror.corp/api/npm/npm-remote HTTPS_PROXY=http://proxy.corp:8080 GOPROXY=https://mirror.corp/api/go
```

**5. pnpm not in the mirror.** `corepack` does **not** help — it fetches pnpm from
the same `registry/pnpm/-/pnpm-*.tgz` path that a strict allowlist mirror 404s. If
your mirror can't serve pnpm, build the UI on the host (`make ui`) and select the
prebuilt stage: `docker build --build-arg UI_STAGE=ui-prebuilt` (or
`WARDYN_UI_STAGE=ui-prebuilt` for the compose build). `make doctor` warns before a
build starts if a TLS-MITM proxy looks present but no corp CA is staged.

## Native-binary agent installs (opt-in; when public npm is blocked)

The agent CLIs install via `npm install -g` by **default**. Behind a corporate
proxy that 403s public npm and where onboarding the package into the internal
mirror is heavy, install the **native binary** instead — no npm, no registry
onboarding. Opt-in per agent; the npm default is unchanged for OSS builds.

`CLAUDE_CODE_VERSION` is an **exact pinned version**, not a channel, and it
drives BOTH install paths (npm takes it as the version specifier; the native
path passes a bare semver through as a release id). A released image has to be
reproducible — a floating `stable`/`latest` means two builds of the same git tag
ship different agents. Channel overrides such as `CLAUDE_CODE_VERSION=stable`
remain available with npm or a host-staged native binary. Native downloads
refuse channels: select an exact version and pin its manifest instead.

**claude-code** (`CLAUDE_INSTALL=native`): installs the native `claude` binary,
checksum-verified against a SHA256-pinned release manifest, from the official
`https://downloads.claude.ai/claude-code-releases` surface — the **only** host this
path contacts (allowlist it in the proxy; the corp CA above covers its TLS):

```
make agent-images-core CLAUDE_INSTALL=native  # the pinned default
make agent-images-core CLAUDE_INSTALL=native CLAUDE_CODE_VERSION=<version> CLAUDE_MANIFEST_SHA256=<sha256>
```

For an alternate version, obtain `manifest.json` from the official release
URL on a trusted host, review it and compute its SHA256. The build checks
that pin before reading the binary checksum from the manifest. Omitting the
pin for an alternate version, or supplying a mismatched pin, fails the build.

For a fully **offline / strict-allowlist** mirror that can't reach
`downloads.claude.ai` from the build either, stage the binary on a host that can,
then build offline (the Dockerfile prefers a staged binary over downloading):

```
scripts/stage-agent-binary.sh claude-code            # downloads + checksum-verifies to deploy/images/claude-code/claude-bin (gitignored)
make agent-images-core CLAUDE_INSTALL=native
```

The script stages the binary for **this host's** architecture. To stage the other one, for
example the arm64 binary from an amd64 build host, pass `--arch amd64|arm64` or set
`WARDYN_AGENT_ARCH` (`x86_64` and `aarch64` also work). The flag beats the variable, and
the variable beats `uname -m`. Checksum verification is unchanged: the release manifest
already lists a sha256 per platform. The build refuses a staged binary built for the other architecture
and names the `--arch` fix.

```
scripts/stage-agent-binary.sh claude-code <version> --arch arm64
```

**codex-cli** (`CODEX_INSTALL=native`): **staged-only** — codex has no
Wardyn-verified public download contract, so the native path consumes a host-staged
binary and fails loudly if it's absent (rather than guessing a URL):

```
WARDYN_CODEX_BIN_URL=<a native codex binary you trust> WARDYN_CODEX_BIN_SHA256=<sha256> \
  scripts/stage-agent-binary.sh codex-cli
make agent-images-core CODEX_INSTALL=native
```

`--arch` is accepted for codex-cli too, but it only labels the log line. The URL is yours,
so it must already point at the matching binary.

Both staged binaries (`deploy/images/{claude-code/claude-bin,codex-cli/codex-bin}`)
are gitignored — never commit them.

## Stop the agent fetching on its own behalf

The claude-code image sets three environment variables, and a corp image that
rebuilds from our Dockerfile inherits all three:

```
CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1
DISABLE_AUTOUPDATER=1
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
```

**`CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1` is the one that
matters in a default-deny estate.** On its first REPL start Claude Code
auto-installs the official plugin marketplace from `downloads.claude.ai`, falling
back to a `git` clone from `github.com` — measured as exactly the two hosts a
first run parks approvals on. Beyond the prompts, this is a supply-chain
decision rather than a convenience: a governed, version-pinned image must not
fetch and install a plugin marketplace at start, because that is third-party code
entering the sandbox outside the artifact you scanned and pinned. Turn it off and
install the plugins you want deliberately, in the image.

`DISABLE_AUTOUPDATER=1` does **not** remove `downloads.claude.ai`. On the npm
install above the update check dials `registry.npmjs.org`, which the shipped
default policy already allows, so it never parked. Set it anyway: a
version-pinned image that upgrades itself mid-run is no longer the artifact you
scanned. A human's `claude update` still works (`DISABLE_UPDATES` blocks that
too, if you want it blocked). `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`
removes the changelog fetch from `raw.githubusercontent.com` plus the
telemetry/error intake. Under `CLAUDE_CODE_USE_BEDROCK=1` what remains is the
Bedrock runtime endpoint and, on the AWS SSO lanes, `oidc.<region>.amazonaws.com`
+ `portal.sso.<region>.amazonaws.com` — added to the run's allowlist by the
control plane itself at dispatch, never by anything inside the sandbox
(`wardyn-aws-sso` is an in-sandbox upload helper; it edits no allowlist).

**This whole section describes an image carrying the three `ENV` lines —
rebuild to get them.** `agent-claude-code` is not a published image (it bundles
Anthropic's Claude Code CLI, which an operator installs under their own
agreement with its vendor); `agent-base` is what ships, and
0.7.6 puts the three lines there too, so any image built `FROM ghcr.io/cjohnstoniv/agent-base:0.7.6`
inherits them. Also needed for the quiet boot, alongside the `ENV` lines: a
`seed_claude_onboarding` call in `agent-run` before the agent starts (product
onboarding only — the workspace-trust and Bypass Permissions prompts are
security questions and are never pre-answered; see
[deploy/images/README.md](../../deploy/images/README.md)). An image on another
base, or an older tag pinned in `WARDYN_AGENT_IMAGES`, still parks
`downloads.claude.ai` and `github.com` on a first interactive run.

**A rebuild is not a rollout.** `make agent-images` tags
`wardyn/agent-claude-code:local` on the machine that built it, which a cluster's
nodes cannot pull. Build with your own tag from the repo root
(`docker build -f deploy/images/claude-code/Dockerfile -t <your-registry>/agent-claude-code:0.7.6 .`),
push it to the registry your nodes pull from, and re-point the `claude-code`
entry of `WARDYN_AGENT_IMAGES` at it — a `helm upgrade` of the control plane
neither rebuilds that image nor moves an entry you have pinned.

**Authoring your own image from scratch** (not rebuilding ours): set all three in
your Dockerfile. Copying our `agent-run` and `agent-run-lib.sh` is **not** a
substitute. The library exports the same three with `${VAR:-1}` defaults, but
that only reaches agent-run's own process tree — task mode and a seeded
interactive run's boot pane. On the DEFAULT interactive run there is no seed, so
`claude` is started by `attach-bashrc.sh` in a fresh attach exec that is not a
descendant of `agent-run`, and only the image `ENV` reaches it. The `:-` defaults
are there so an operator who deliberately wants one of these can set it to `0` on
the run.

## Multi-arch images

Every image Wardyn publishes is a `linux/amd64` + `linux/arm64` index (`platforms:` in [`release.yml`](../../.github/workflows/release.yml)). A run's image ref is resolved by whichever computer runs it, so one ref must work on an ARM laptop and an x86 cluster.

- Keep every ref in [`WARDYN_AGENT_IMAGES`](../ENV.md) (and every image you build from our bases) a multi-arch index with both platforms.
- Pin an index by its **index digest** (`repo@sha256:…` from `docker buildx imagetools inspect <ref>`), never by a child-manifest digest.
- A multi-arch index that lacks a platform fails at pull on that architecture (`no matching manifest for linux/arm64/v8`). A single-architecture image pulls anywhere and fails only when it starts on the other architecture, unless the engine emulates it. A laptop runner starts such an image once to check and refuses it, naming the image, when it cannot run (`image_arch`).
- If your mirror publishes one architecture per tag, name both: `{"base": "<mirror>/agent-base:<tag>", "base@arm64": "<mirror>/agent-base:<tag>-arm64"}` in `WARDYN_AGENT_IMAGES`. A run on an `arm64` runner uses the `@arm64` entry; every other run uses the plain one. Set the proxy the same way with `WARDYN_PROXY_IMAGE_ARM64` (and `_AMD64`); a runner is handed the one for its architecture when it registers.
- Build and push both platforms in one command, then check the index lists both:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f deploy/images/claude-code/Dockerfile \
  -t <your-registry>/agent-claude-code:<tag> --push .
docker buildx imagetools inspect <your-registry>/agent-claude-code:<tag>
```

### Cross-arch builds when the public registry is blocked

The `buildx` recipe above depends on the public registry **twice**. On an amd64 build host whose proxy refuses it (HTTP 403), both halves fail:

| Step | What it pulls | Failure |
|---|---|---|
| Emulation, `docker run --privileged tonistiigi/binfmt --install arm64` | the binfmt image | 403, exit 125 |
| `docker buildx create`, which provisions a container builder | the builder's `moby/buildkit` image | 403 |

The second one is the confusing one. Skipping emulation does not help, because the build fails earlier, at `booting buildkit`, before the Dockerfile is read. The 403 body is the proxy's HTML block page, so the error reads `invalid character '<'` and looks like a JSON bug.

Cross-arch does not require running target-arch instructions. Run them only in a native stage and keep the target stage free of `RUN`. Plain `docker build --platform linux/arm64` then cross-builds on the daemon's embedded BuildKit and pulls nothing extra:

```dockerfile
# Both ARGs MUST precede the first FROM, else they are stage-scoped and blank.
ARG CLI_STAGE_IMAGE
ARG BASE_IMAGE
# Native stage: any mirrored image that has gunzip.
FROM --platform=$BUILDPLATFORM ${CLI_STAGE_IMAGE} AS cli
ARG CLAUDE_SHA256=<sha256 of the linux-arm64 binary from the release manifest>
# A gzip of that binary, next to the Dockerfile.
COPY claude-linux-arm64.gz /tmp/claude.gz
RUN set -eu; gunzip -c /tmp/claude.gz > /tmp/claude; echo "$CLAUDE_SHA256  /tmp/claude" | sha256sum -c -
# Zero RUN from here on, so nothing needs emulation.
FROM ${BASE_IMAGE}
COPY --chmod=0755 --from=cli /tmp/claude /usr/local/bin/claude
COPY --chmod=0755 agent-run /usr/local/bin/agent-run
```

```sh
DOCKER_BUILDKIT=1 docker build --platform linux/arm64 \
  --build-arg BASE_IMAGE=<your-registry>/agent-base:<tag>-arm64 \
  --build-arg CLI_STAGE_IMAGE=<your-registry>/<any-image-with-gunzip>:<tag> -t <ref> .
test "$(docker image inspect --format '{{.Architecture}}' <ref>)" = arm64   # nothing executed, so assert
```

- **The assertion replaces the build-time proof.** Nothing ran for arm64, so no `claude --version` check happened. A wrong `BASE_IMAGE` otherwise ships an amd64 image silently, and it fails only on a laptop.
- **Do not `rm` the archive.** `/tmp` is sticky and the `COPY`ed file is root-owned, so an unprivileged `USER` gets `Operation not permitted`.
- **The shipped Dockerfile does not have this shape.** [`deploy/images/claude-code/Dockerfile`](../../deploy/images/claude-code/Dockerfile) runs `RUN` steps in its final stage. The shape above applies to a layer you add on a published multi-arch base.
