# wardyn/agent-campaign: the convention claude-code agent image + the campaign's
# real language toolchains (Go, Python, Rust, JDK21/Maven; Node/npm already in
# the base). Sidesteps envbuild entirely: record/verify AND contributor runs all
# resolve to ONE image that carries BOTH the wardyn binaries (agent-run,
# wardyn-verify, wardyn-rec, wardyn-scan, claude, git — inherited from the base)
# AND the toolchains, so real build/test commands run in-sandbox.
FROM wardyn/agent-claude-code:local

# SSH SCM lane note: openssh-client + corkscrew (CONNECT ProxyCommand helper) and
# the baked /etc/ssh/ssh_known_hosts are INHERITED from the base image above — no
# re-install here (they are one FROM away). agent-run (also inherited) wires the
# git-over-SSH-over-443 clone when a run has an ssh_key grant. See GrantSSHKey.

USER root
ENV DEBIAN_FRONTEND=noninteractive

# Python (pure-python projects: pip + venv + a compiler for the odd sdist).
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv python3-dev build-essential \
      pkg-config ca-certificates curl wget gnupg unzip \
    && rm -rf /var/lib/apt/lists/*

# Go (tarball to /usr/local; GOPATH/GOCACHE land in the agent's writable HOME).
#
# MUST satisfy the go.mod of the repos this image builds. GOTOOLCHAIN=local (below)
# deliberately FORBIDS the auto-download of a newer toolchain — the corporate /
# air-gapped posture — so a Go older than a repo's `go` directive does not degrade,
# it FAILS: "go.mod requires go >= 1.26 (running go 1.23.5; GOTOOLCHAIN=local)".
# Keep this at or above the `toolchain` line of the repos you build (wardyn itself
# pins toolchain go1.26.4). Bump here rather than relaxing GOTOOLCHAIN.
ARG GO_VERSION=1.26.4
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/home/agent/go/bin:${PATH} \
    GOPATH=/home/agent/go GOTOOLCHAIN=local

# pnpm. The base image ships node/npm/yarn/corepack but NOT pnpm, so any repo whose
# package.json pins `"packageManager": "pnpm@x"` (wardyn's ui/ does) fails its very
# first setup command with `pnpm: No such file or directory`. Baked in globally
# rather than left to a corepack shim, so no registry fetch is needed at RUN time
# (the sandbox may have npm egress denied, and an air-gapped host has none at all).
ARG PNPM_VERSION=9.15.4
RUN npm install -g "pnpm@${PNPM_VERSION}" && pnpm --version

# JDK 21 (Temurin) + Maven, for jsoup (needs 21) and JVM projects generally.
RUN install -d /etc/apt/keyrings \
    && wget -qO - https://packages.adoptium.net/artifactory/api/gpg/key/public | gpg --dearmor -o /etc/apt/keyrings/adoptium.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb bookworm main" > /etc/apt/sources.list.d/adoptium.list \
    && apt-get update && apt-get install -y --no-install-recommends temurin-21-jdk maven \
    && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/usr/lib/jvm/temurin-21-jdk-amd64

# Rust (rustup into a world-writable shared location so the uid-1000 agent can
# build; the base image runs as USER agent with no root exec to write ~/.cargo).
ENV RUSTUP_HOME=/opt/rust CARGO_HOME=/opt/rust PATH=/opt/rust/bin:${PATH}
RUN curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain 1.82.0 \
    && chmod -R a+rwX /opt/rust

# wardyn-verify runs setup commands via `/bin/sh -lc` — a LOGIN shell, which
# sources /etc/profile and RESETS PATH to the system default, dropping the Docker
# ENV PATH additions (go, rust). Re-export the toolchain PATH + homes from
# /etc/profile.d so login shells see them (java/mvn/python3 already live under
# /usr/bin which the default PATH includes; only go + rust need this).
# GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles each package's
# test binary into $TMPDIR and then EXECS it — which fails "permission denied" on
# a noexec /tmp. Point it at the agent's exec-allowed HOME instead.
RUN install -d -o agent -g agent /home/agent/.gotmp /home/agent/go /home/agent/.cache/go-build

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
RUN printf '%s\n' \
      'export PATH=/usr/local/go/bin:/opt/rust/bin:/home/agent/go/bin:$PATH' \
      'export GOPATH=/home/agent/go' \
      'export GOCACHE=/home/agent/.cache/go-build' \
      'export GOTMPDIR=/home/agent/.gotmp' \
      'export GOTOOLCHAIN=local' \
      'export CARGO_HOME=/opt/rust' \
      'export RUSTUP_HOME=/opt/rust' \
      'export JAVA_HOME=/usr/lib/jvm/temurin-21-jdk-amd64' \
      > /etc/profile.d/campaign-toolchains.sh

# Sanity: every toolchain resolves under BOTH a plain and a LOGIN shell (the
# latter is how wardyn-verify invokes setup commands).
RUN node --version && npm --version && python3 --version && go version \
    && java -version && mvn -v | head -1 && rustc --version && cargo --version \
    && command -v claude && command -v agent-run && command -v wardyn-verify \
    && sh -lc 'go version && cargo --version && mvn -v | head -1 && python3 --version'

USER agent
# Per-project package caches live under the agent's writable HOME.
ENV GOCACHE=/home/agent/.cache/go-build CARGO_HOME=/opt/rust
