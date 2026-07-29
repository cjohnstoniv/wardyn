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

**2. Go builds:** add `ENV GOTOOLCHAIN=local` so the pinned-toolchain self-upgrade
fetch (blocked behind a MITM proxy) is skipped; the corp CA above lets
`go mod download` verify the module proxy.

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
`NPM_REGISTRY`/`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` into every `docker build` via
`$(DOCKER_BUILD_ARGS)`; the compose stanzas pass them through `build.args`. So a
corp user runs, e.g.:

```
make agent-images NPM_REGISTRY=https://mirror.corp/api/npm/npm-remote HTTPS_PROXY=http://proxy.corp:8080
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

**claude-code** (`CLAUDE_INSTALL=native`): installs the native `claude` binary,
checksum-verified against the release manifest, from the official
`https://downloads.claude.ai/claude-code-releases` surface — the **only** host this
path contacts (allowlist it in the proxy; the corp CA above covers its TLS):

```
make agent-images-core CLAUDE_INSTALL=native                      # stable channel
make agent-images-core CLAUDE_INSTALL=native CLAUDE_CODE_VERSION=2.1.215   # pinned
```

For a fully **offline / strict-allowlist** mirror that can't reach
`downloads.claude.ai` from the build either, stage the binary on a host that can,
then build offline (the Dockerfile prefers a staged binary over downloading):

```
scripts/stage-agent-binary.sh claude-code            # downloads + checksum-verifies to deploy/images/claude-code/claude-bin (gitignored)
make agent-images-core CLAUDE_INSTALL=native
```

**codex-cli** (`CODEX_INSTALL=native`): **staged-only** — codex has no
Wardyn-verified public download contract, so the native path consumes a host-staged
binary and fails loudly if it's absent (rather than guessing a URL):

```
WARDYN_CODEX_BIN_URL=<a native codex binary you trust> WARDYN_CODEX_BIN_SHA256=<sha256> \
  scripts/stage-agent-binary.sh codex-cli
make agent-images-core CODEX_INSTALL=native
```

Both staged binaries (`deploy/images/{claude-code/claude-bin,codex-cli/codex-bin}`)
are gitignored — never commit them.
