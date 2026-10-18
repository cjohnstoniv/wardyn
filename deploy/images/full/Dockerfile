# wardyn/agent-full: the convention claude-code agent image + the full
# real language toolchains (Go, Python, Rust, JDK21/Maven; Node/npm already in
# the base). Sidesteps envbuild entirely: record AND contributor runs all
# resolve to ONE image that carries BOTH the wardyn binaries (agent-run,
# wardyn-rec, wardyn-scan, claude, git — inherited from the base)
# AND the toolchains, so real build/test commands run in-sandbox.

# Toolchain SOURCE stages. Go and Rust are COPYed out of their digest-pinned
# official images rather than downloaded at build time: the Go tarball came down
# with no checksum and rustup was a bare `curl | sh`, while every sibling image
# verifies what it fetches (claude-code sha256sum's the CLI, aws-sso gpg-verifies
# the AWS CLI). A digest also beats a hand-written ARG *_SHA256 on upkeep —
# .github/dependabot.yml:33 already watches this directory for the docker
# ecosystem, so these pins get bumped for us; a SHA constant would rot unwatched.
#
# GO MUST satisfy the go.mod of the repos this image builds. GOTOOLCHAIN=local
# (below, and already set in the golang image) deliberately FORBIDS the
# auto-download of a newer toolchain — the corporate / air-gapped posture — so a Go
# older than a repo's `go` directive does not degrade, it FAILS: "go.mod requires
# go >= 1.26 (running go 1.23.5; GOTOOLCHAIN=local)". Keep this at or above the
# `toolchain` line of the repos you build (this digest is go1.27.2; wardyn itself
# pins toolchain go1.26.9). Bump the pin rather than relaxing GOTOOLCHAIN.
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS goroot
FROM rust:1.99.0-slim-trixie@sha256:2752b332db73fdbb7dc576f06c82ed1f312005784ef913d7e04a28f5f55dc581 AS rustroot

# Not digest-pinned: this is a locally-built image tag (produced by
# deploy/images/claude-code/Dockerfile), not a registry image — it has no
# resolvable upstream digest and is rebuilt+retagged on every local build.
FROM wardyn/agent-claude-code:local

# Local build recipe only — never in release.yml's publish matrix (a gate in
# scripts/check-image-pins.sh keeps it out). The licences label extends the
# inherited base expression with what THIS stage adds: Go (BSD-3-Clause),
# Rust (MIT OR Apache-2.0), CPython (PSF-2.0), Temurin JDK (GPL-2.0 WITH
# Classpath-exception-2.0), Maven (Apache-2.0), build-essential (GPL), the
# Azure CLI (MIT).
LABEL org.opencontainers.image.title="agent-full" \
      org.opencontainers.image.description="Wardyn agent image: claude-code plus full language toolchains (local build only)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND BSD-3-Clause AND MIT AND PSF-2.0 AND GPL-2.0-only WITH Classpath-exception-2.0 AND LicenseRef-Anthropic-Terms" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn"

# SSH SCM lane note: openssh-client + corkscrew (CONNECT ProxyCommand helper) and
# the baked /etc/ssh/ssh_known_hosts are INHERITED from the base image above — no
# re-install here (they are one FROM away). agent-run (also inherited) wires the
# git-over-SSH-over-443 clone when a run has an ssh_key grant. See GrantSSHKey.

USER root
ENV DEBIAN_FRONTEND=noninteractive

# Corporate build support, same shape as every sibling image (claude-code,
# codex-cli, aws-sso, oracle): build-only ARGs, never ENV — a persistent proxy
# ENV would leak into agent RUNS, whose only egress is wardyn-proxy. Declared
# here at the top of the stage so the apt/wget layers below see them too.
# The corp CA is NOT re-copied: `FROM wardyn/agent-claude-code:local` already
# carries that image's update-ca-certificates layer and NODE_EXTRA_CA_CERTS, so
# a second COPY would be a redundant copy of the same pem.
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=

# Python (pure-python projects: pip + venv + a compiler for the odd sdist).
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv python3-dev build-essential \
      pkg-config ca-certificates curl wget gnupg unzip \
    && rm -rf /var/lib/apt/lists/*

# Go (from the goroot stage; GOPATH/GOCACHE land in the agent's writable HOME).
# Version rationale + bump instructions live on the goroot FROM at the top.
COPY --from=goroot /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/home/agent/go/bin:${PATH} \
    GOPATH=/home/agent/go GOTOOLCHAIN=local

# pnpm. The base image ships node/npm/yarn/corepack but NOT pnpm, so any repo whose
# package.json pins `"packageManager": "pnpm@x"` (wardyn's ui/ does) fails its very
# first setup command with `pnpm: No such file or directory`. Baked in globally
# rather than left to a corepack shim, so no registry fetch is needed at RUN time
# (the sandbox may have npm egress denied, and an air-gapped host has none at all).
# Honors NPM_REGISTRY/HTTP(S)_PROXY like the sibling images' CLI installs — this
# was the one build layer that always dialed public npm, so `make agent-image-full`
# was the one build that failed behind a corp proxy or an allowlisted mirror.
ARG PNPM_VERSION=9.15.4
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi; \
    if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi; \
    if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi; \
    npm install -g "pnpm@${PNPM_VERSION}" && pnpm --version

# JDK 21 (Temurin) + Maven, for jsoup (needs 21) and JVM projects generally.
ARG TARGETARCH
RUN install -d /etc/apt/keyrings \
    && wget -qO - https://packages.adoptium.net/artifactory/api/gpg/key/public | gpg --dearmor -o /etc/apt/keyrings/adoptium.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb trixie main" > /etc/apt/sources.list.d/adoptium.list \
    && apt-get update && apt-get install -y --no-install-recommends temurin-21-jdk maven \
    && rm -rf /var/lib/apt/lists/* \
    && ln -s "/usr/lib/jvm/temurin-21-jdk-${TARGETARCH:-$(dpkg --print-architecture)}" /usr/lib/jvm/temurin-21-jdk
# The JDK installs under an architecture-suffixed directory (-amd64, -arm64); the arch-neutral link keeps JAVA_HOME one value.
ENV JAVA_HOME=/usr/lib/jvm/temurin-21-jdk

# ── Azure CLI (az) (the Azure CLI component, cli.kind azure) ─────────────────
# From Microsoft's apt repository. The repository's signing key is checked by
# FINGERPRINT (Microsoft (Release signing), key id EB3E94ADBE1229CF) rather than
# by a digest of the key file, which changes whenever Microsoft re-exports the
# key; a key that is not that one, or a file carrying a second key, fails the
# build. signed-by makes the key vouch for this repository alone, and the
# version is pinned: a bump moves AZURE_CLI_VERSION here and in every other
# image that carries this block (scripts/test-image-recipes.py fails when the
# copies drift). The suite is bookworm on this trixie base: Microsoft publishes
# azure-cli for bookworm and for no later Debian release, and the package
# carries its own Python, so it installs and runs here unchanged. gnupg and
# wget are removed again unless the image already had them. az gets no Azure
# credential here or at run time: a run with an Azure CLI component gets a
# profile and an inert token cache, and wardyn-proxy adds the token to each
# request it lets through.
ARG AZURE_CLI_VERSION=2.91.0
RUN set -eu; \
    had_gpg=1; command -v gpg >/dev/null || had_gpg=0; \
    had_wget=1; command -v wget >/dev/null || had_wget=0; \
    apt-get update; apt-get install -y --no-install-recommends ca-certificates gnupg wget; \
    install -d /etc/apt/keyrings; export GNUPGHOME=/tmp/az-gnupg; install -d -m 0700 "$GNUPGHOME"; \
    wget -qO /tmp/microsoft.asc https://packages.microsoft.com/keys/microsoft.asc; \
    keys="$(gpg --show-keys --with-colons /tmp/microsoft.asc)"; \
    [ "$(printf '%s\n' "$keys" | grep -c '^pub:')" = 1 ] || { echo "Microsoft's package signing key file does not hold exactly one key" >&2; exit 1; }; \
    fpr="$(printf '%s\n' "$keys" | awk -F: '$1 == "fpr" { print $10; exit }')"; \
    [ "$fpr" = "BC528686B50D79E339D3721CEB3E94ADBE1229CF" ] || { echo "Microsoft's package signing key is not the pinned one: $fpr" >&2; exit 1; }; \
    gpg --dearmor -o /etc/apt/keyrings/microsoft.gpg /tmp/microsoft.asc; \
    rm -rf /tmp/microsoft.asc "$GNUPGHOME"; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/microsoft.gpg] https://packages.microsoft.com/repos/azure-cli/ bookworm main" > /etc/apt/sources.list.d/azure-cli.list; \
    # Microsoft's repository is trusted for azure-cli only, never for any other package name.
    printf 'Package: *\nPin: origin packages.microsoft.com\nPin-Priority: -1\n\nPackage: azure-cli\nPin: origin packages.microsoft.com\nPin-Priority: 500\n' > /etc/apt/preferences.d/azure-cli; \
    apt-get update; apt-get install -y --no-install-recommends "azure-cli=${AZURE_CLI_VERSION}-1~bookworm"; \
    [ "$had_gpg" = 1 ] || apt-get purge -y --auto-remove gnupg; \
    [ "$had_wget" = 1 ] || apt-get purge -y --auto-remove wget; \
    rm -rf /var/lib/apt/lists/*; \
    command -v az

# Rust (from the rustroot stage, into a world-writable shared location so the
# uid-1000 agent can build; the base image runs as USER agent with no root exec to
# write ~/.cargo). RUSTUP_HOME and CARGO_HOME both point at /opt/rust — the same
# single-directory layout rustup-init produces when both are set to it, and the two
# source trees do not overlap (rustup owns toolchains/, cargo owns bin/). The chmod
# is still needed: COPY creates /opt/rust itself root-owned 0755, so cargo could not
# write its registry cache there (the copied contents are already a+w).
ENV RUSTUP_HOME=/opt/rust CARGO_HOME=/opt/rust PATH=/opt/rust/bin:${PATH}
COPY --from=rustroot /usr/local/rustup/ /opt/rust/
COPY --from=rustroot /usr/local/cargo/ /opt/rust/
RUN chmod -R a+rwX /opt/rust

# The exec task mode (WARDYN_TASK_MODE=exec, deploy/images/common/agent-run-lib.sh
# maybe_exec_task_mode) runs its command via `/bin/sh -lc` — a LOGIN shell, which
# sources /etc/profile and RESETS PATH to the system default, dropping the Docker
# ENV PATH additions (go, rust). Re-export the toolchain PATH + homes from
# /etc/profile.d so login shells see them (java/mvn/python3 already live under
# /usr/bin which the default PATH includes; only go + rust need this).
# GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles each package's
# test binary into $TMPDIR and then EXECS it — which fails "permission denied" on
# a noexec /tmp. Point it at the agent's exec-allowed HOME instead — under
# .cache alongside GOCACHE and GOMODCACHE (not GOPATH itself, so the installed
# tool binaries under go/bin stay put) so a k8s run's fill lands on the volume
# ephemeralScratchVolumes mounts there (internal/runner/k8s/naming.go) and
# counts against disk_mib instead of the pod's unmetered writable layer.
# The parents are named so they get the owner too: `install -d` leaves the ones it
# creates implicitly (.cache, .cache/go) root-owned, and the agent then cannot create
# ~/.cache/ms-playwright, node-gyp's devdir or pip's cache.
RUN install -d -o agent -g agent /home/agent/go /home/agent/.cache /home/agent/.cache/go \
      /home/agent/.cache/go-build /home/agent/.cache/gotmp /home/agent/.cache/go/mod \
      /home/agent/.cache/npm

# Maven proxy: unlike npm/pip/cargo/go/git, Maven does NOT honor the sandbox's
# HTTP(S)_PROXY env vars — it resolves Maven Central directly and fails
# "Unknown host repo.maven.apache.org" because the sandbox has no direct egress
# (only the wardyn-proxy sidecar). A user-level settings.xml routes all Maven
# repository traffic through the proxy (protocol=http tunnels https via CONNECT;
# loopback excluded so jsoup's local integration-test server isn't proxied).
RUN install -d -o agent -g agent /home/agent/.m2 && \
    printf '%s\n' \
      '<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">' \
      '  <proxies>' \
      '    <proxy>' \
      '      <id>wardyn</id><active>true</active>' \
      '      <protocol>http</protocol><host>wardyn-proxy</host><port>3128</port>' \
      '      <nonProxyHosts>localhost|127.0.0.1|::1</nonProxyHosts>' \
      '    </proxy>' \
      '  </proxies>' \
      '</settings>' > /home/agent/.m2/settings.xml && \
    chown agent:agent /home/agent/.m2/settings.xml
# These MUST match dispatch's own toolchain env (buildBaseSandboxEnv,
# internal/api/runs_dispatch_mounts.go) byte-for-byte: a login shell (exec task
# mode's `/bin/sh -lc`, sourced above) re-exports every one of these AFTER
# dispatch's env lands, so a path named here that disagreed with dispatch would
# silently win and undo the k8s cache-volume relocation (issue #164) the moment
# a task ran in a login shell. GOPATH itself is unchanged (only GOMODCACHE
# moves) so the installed tool binaries under its bin/ stay reachable.
RUN printf '%s\n' \
      'export PATH=/usr/local/go/bin:/opt/rust/bin:/home/agent/go/bin:$PATH' \
      'export GOPATH=/home/agent/go' \
      'export GOCACHE=/home/agent/.cache/go-build' \
      'export GOTMPDIR=/home/agent/.cache/gotmp' \
      'export GOMODCACHE=/home/agent/.cache/go/mod' \
      'export GOTOOLCHAIN=local' \
      'export CARGO_HOME=/opt/rust' \
      'export RUSTUP_HOME=/opt/rust' \
      'export JAVA_HOME=/usr/lib/jvm/temurin-21-jdk' \
      'export npm_config_cache=/home/agent/.cache/npm' \
      > /etc/profile.d/toolchains.sh

# Sanity: every toolchain resolves under BOTH a plain and a LOGIN shell (the
# latter is how exec task mode invokes its command).
RUN node --version && npm --version && python3 --version && go version \
    && java -version && mvn -v | head -1 && rustc --version && cargo --version \
    && command -v claude && command -v agent-run \
    && sh -lc 'go version && cargo --version && mvn -v | head -1 && python3 --version'

USER agent
# Per-project package caches live under the agent's writable HOME — set again
# here (not just in /etc/profile.d/toolchains.sh above) so a PLAIN shell run
# outside Wardyn dispatch (which would otherwise set these itself, see
# buildBaseSandboxEnv) still gets sane, disk_mib-eligible defaults.
ENV GOCACHE=/home/agent/.cache/go-build \
    GOTMPDIR=/home/agent/.cache/gotmp \
    GOMODCACHE=/home/agent/.cache/go/mod \
    npm_config_cache=/home/agent/.cache/npm \
    CARGO_HOME=/opt/rust
