# wardyn/agent-codex-cli: OpenAI Codex CLI coding-agent image.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from the repository root.
# Stage 2: node:24-trixie-slim installs Codex CLI plus OS utilities.
#
# IMAGE CONTRACT (all agent images must satisfy these — see deploy/images/README.md):
#   - NO ENTRYPOINT: the Wardyn docker driver sets Cmd=["sleep","infinity"].
#   - /usr/local/bin/agent-run "<task>": runs one agent task non-interactively.
#   - /usr/local/bin/wardyn-rec: session recorder.
#   - /usr/local/bin/wardyn-git-helper: git credential helper.
#   - USER agent (uid 1000), home /home/agent, workspace /home/agent/work.
#   - System gitconfig wires the credential helper GLOBALLY (all hosts).
#
# LLM BROKERING NOTE:
#   The Wardyn sandbox env must NOT contain secrets (invariant 1), so the real
#   key never reaches it: the proxy mints it at startup and injects it on the
#   brokered openai LLM route (/wardyn/llm/openai/<rest>, local_routes.go).
#   Codex 0.149.1 reads neither OPENAI_BASE_URL nor OPENAI_API_KEY. Dispatch
#   (runs_dispatch_provider.go) sets WARDYN_CODEX_BASE_URL, the route plus the
#   path its upstream needs, and CODEX_API_KEY, the inert sentinel
#   `wardyn-proxy-injected`; agent-run's prepare_codex_config writes the first
#   into config.toml as `openai_base_url` and logs the sentinel in for the
#   interactive tool, so the sandbox holds only the sentinel. OPENAI_BASE_URL
#   and OPENAI_API_KEY are still set, for an operator image with an older codex.
#   This image sets none of them: with no route configured codex has none to
#   reach, which is the correct default.

# ── Stage 1: Go builder ───────────────────────────────────────────────────────
# --platform=$BUILDPLATFORM: runs on the BUILDER's arch and cross-compiles to
# $TARGETARCH below (pure Go, CGO_ENABLED=0) — no QEMU for the compile. Stage 2
# does need it (apt + npm run as the target arch).
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder

WORKDIR /build

# Corporate build support (no-ops on an OSS build): GOTOOLCHAIN=local skips the
# pinned-toolchain self-upgrade fetch (blocked behind a TLS-MITM proxy); the
# optional corp CA (host-staged deploy/images/corp-ca.pem, gitignored) is added to
# the system trust so `go mod download` can verify the module proxy through
# corporate MITM. The README.md companion makes the COPY succeed when the
# corp-ca.pem* glob matches nothing.
ENV GOTOOLCHAIN=local
# Corporate Go module mirror (Athens/Artifactory/Nexus). Empty is identical to
# unset: the go command falls back to $GOROOT/go.env (https://proxy.golang.org,direct).
# GOSUMDB is deliberately not exposed — every mainstream mirror proxies /sumdb/;
# add the same two lines if one ever does not.
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETARCH is BuildKit's automatic per-target ARG; empty under the legacy
# builder, and `GOARCH=` empty is how Go spells "use the default".
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-rec ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-git-helper ./cmd/wardyn-git-helper \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-scan ./cmd/wardyn-scan

# ── Stage 2: agent runtime ────────────────────────────────────────────────────
FROM node:24-trixie-slim@sha256:8ec5d7557396cfe32d21c3f9c13072355ceab22b584578ca4bb28af31120cffe

# Apache-2.0 s4(a) requires a copy of the Licence to accompany the distribution and
# s4(d) requires the NOTICE contents to be carried. Publishing an image IS
# distribution, so they ship inside it. THIRD-PARTY-NOTICES.md carries the
# MIT/BSD/ISC/OFL notices every bundled dependency requires.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/
# The VERBATIM texts, not just the index. MIT, BSD, ISC and OFL-1.1 each require
# their copyright and permission notice to accompany the copy; a table of licence
# names and URLs is not that notice. licenses/texts/ is ~1.8M and is the only
# thing here that actually discharges the condition.
COPY licenses/texts/ /usr/share/doc/wardyn/licenses/

# org.opencontainers.image.licenses states what is ACTUALLY in this image, not what
# Wardyn's own code is licensed under -- enterprise registries and scanners key off
# it, and a bare "Apache-2.0" here would be a false declaration.
LABEL org.opencontainers.image.title="agent-codex-cli" \
      org.opencontainers.image.description="Wardyn agent image: OpenAI Codex CLI" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn" \
      org.opencontainers.image.documentation="https://github.com/cjohnstoniv/wardyn/blob/main/THIRD-PARTY-NOTICES.md"


# tmux: the interactive attach shell falls back through tmux -> bash -> sh
# (deploy/images/common/attach-bashrc.sh); codex-cli had none, unlike
# claude-code. openssh-sftp-server + socat: the SSH-gateway lane (see
# docs/SSH.md; wired by the SSH gateway itself, not built here) needs an
# in-sandbox SFTP subsystem (/usr/lib/openssh/sftp-server) and a
# general-purpose relay (/usr/bin/socat) — see deploy/images/README.md's "SSH
# relay binaries" note for the exact paths. openssh-sftp-server pulls in
# openssh-client (the `ssh` binary) transitively — NOT requested, harmless
# unused — but this image still does NOT wire it: no baked known_hosts, no
# corkscrew (SSH-over-443 proxy tunnel), no GIT_SSH_COMMAND from agent-run.
# This image's git-over-SSH clone story is unchanged (git-over-HTTPS only).
# apt-get upgrade: take the current security updates for packages the pinned base already carries.
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        git \
        curl \
        ca-certificates \
        asciinema \
        tmux \
        openssh-sftp-server \
        socat \
    && rm -rf /var/lib/apt/lists/*

# Corporate build support (no-ops on OSS): install the optional corp CA into the
# system + node trust so `npm install -g` can verify registry TLS through a corp
# MITM proxy. The proxy/registry knobs are build-only ARGs (NOT ENV) — a persistent
# proxy ENV would leak into agent RUNS, whose only egress is wardyn-proxy.
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi
ENV NODE_EXTRA_CA_CERTS=/corp-ca/corp-ca.pem

# Install the Codex CLI. DEFAULT: npm (@openai/codex, installs the `codex`
# binary). Behind a corp proxy where public npm is blocked, set CODEX_INSTALL=native
# to install a HOST-STAGED native `codex` binary instead (stage it at
# deploy/images/codex-cli/codex-bin — e.g. via scripts/stage-agent-binary.sh with
# a WARDYN_CODEX_BIN_URL you control). Unlike claude-code, codex has no
# Wardyn-verified public download contract, so the native path is staged-only:
# the build fails loudly if native is selected without a staged binary rather than
# guessing a URL. See docs/adoption/corp-image-authoring.md "Native-binary agent installs".
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
ARG CODEX_INSTALL=npm
# CODEX_VERSION is a PINNED exact version, exactly like CLAUDE_CODE_VERSION in
# the claude-code sibling and AWS_CLI_VERSION in aws-sso. This image IS
# published and cosign-signed (.github/workflows/release.yml's agent-codex-cli
# row), so an unversioned `npm install -g @openai/codex` meant two builds of
# the SAME git tag shipped different agents — the reproducibility rule its two
# siblings state verbatim (D3, supply chain), and the one third-party agent CLI
# in the image set that was exempt from it.
#
# The value is the version deploy/images/THIRD-PARTY-TERMS.md already records
# as the one this image conveys. That table is a LICENCE statement about a
# specific version, so bump both in the same commit — deliberately, never as a
# side effect of a rebuild.
ARG CODEX_VERSION=0.149.1
# Same CVE-2026-59873 fix as the claude-code sibling: the base image's bundled
# npm (10.9.8) vendors node-tar 7.5.11. 11.19.1 vendors 7.5.22, which is also
# the floor for CVE-2026-73566 (node-tar <= 7.5.20, patched 7.5.21) — 11.19.0
# vendors 7.5.19 and so does 12.0.2, so a HIGHER npm is not automatically a
# fixed one. No node:24-trixie-slim digest ships a fixed one yet, and it is
# npm's internal copy, so upgrading npm is the only reach. Keep in step with
# that image.
ARG NPM_VERSION=11.19.1
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && npm install -g "npm@${NPM_VERSION}" \
 && npm --version
COPY deploy/images/README.md deploy/images/codex-cli/codex-bin* /codex-stage/
RUN set -eu; \
    if [ "$CODEX_INSTALL" = "native" ]; then \
        [ -f /codex-stage/codex-bin ] || { echo "CODEX_INSTALL=native but deploy/images/codex-cli/codex-bin is not staged — stage it first (see deploy/images/README.md)" >&2; exit 1; }; \
        install -m 0755 /codex-stage/codex-bin /usr/local/bin/codex; \
        echo "installed host-staged native codex"; \
    else \
        if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi; \
        if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi; \
        if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi; \
        npm install -g "@openai/codex@$CODEX_VERSION"; \
        echo "installed codex $CODEX_VERSION via npm"; \
    fi

# The node:24-trixie-slim base image ships a `node` user at uid=1000.
# Rename it to `agent` and relocate the home directory so the sandbox contract
# is satisfied without creating a conflicting second uid-1000 user.
# /home/agent/drive is the RESERVED user-drive mount point (runner.DriveTarget),
# created here owned by agent for the copy-up reason deploy/images/base/Dockerfile
# states in full: a fresh managed volume inherits the uid/gid of the image directory
# it is mounted over, so without it a docker_volume drive's root is root-owned and a
# writable managed drive is read-only in practice for uid 1000.
# chmod 0755: Debian 13 creates the home 0700 (Debian 12 gave 0755); a run under
# another uid (the docker-tagged tests, platforms that assign one) must reach it.
RUN usermod  -l agent node \
 && usermod  -d /home/agent -m agent \
 && groupmod -n agent node \
 && mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent \
 && chmod 0755 /home/agent

# Copy the statically-linked binaries from the build stage.
COPY --from=builder /out/wardyn-rec        /usr/local/bin/wardyn-rec
COPY --from=builder /out/wardyn-git-helper /usr/local/bin/wardyn-git-helper
# wardyn-scan: in-sandbox workspace scanner run by a governed scan run.
COPY --from=builder /out/wardyn-scan       /usr/local/bin/wardyn-scan

# agent-run launcher.
# agent-run-lib.sh: functions shared with every other agent image's agent-run.
# tmux config for the persistent attach session: no status bar (clean terminal,
# no xterm.js line-drawing artifacts), truecolor, tmux-owned mouse wheel, large scrollback.
COPY deploy/images/common/tmux.conf /etc/tmux.conf
COPY deploy/images/common/agent-run-lib.sh /usr/local/bin/agent-run-lib.sh
COPY deploy/images/codex-cli/agent-run /usr/local/bin/agent-run
# Interactive attach shell sources this: waits for the workspace clone to finish +
# cd's into it, so a mid-clone attach never shows an empty ~/work.
COPY --chown=agent:agent deploy/images/common/attach-bashrc.sh /home/agent/.bashrc
RUN chmod +x /usr/local/bin/wardyn-rec \
           /usr/local/bin/wardyn-git-helper \
           /usr/local/bin/wardyn-scan \
           /usr/local/bin/agent-run

# System-wide GLOBAL git credential helper (all hosts).
# It brokers github.com (WARDYN_GITHUB_GRANT_ID) AND matched non-GitHub hosts
# (WARDYN_GIT_PAT_GRANTS, e.g. Azure DevOps / GitLab); for any other host it
# emits nothing and git falls through, so a GLOBAL helper is safe.
# --secret-file wires the per-run CALLER-AUTH gate: before emitting a credential
# the helper requires the caller to present WARDYN_GIT_HELPER_SECRET matching
# this 0400 agent-owned file (provisioned by agent-run at task time).  This
# config is in the ROOT-owned system gitconfig so the agent cannot rewrite the
# path.  When the file is absent (e.g. an interactive run that never ran
# agent-run) the helper falls open and mints, so legitimate git is never blocked.
RUN git config --system credential.helper \
        '/usr/local/bin/wardyn-git-helper --secret-file /home/agent/.wardyn/git-helper.secret'

WORKDIR /home/agent/work

USER agent

# The node:24-trixie-slim base image sets ENTRYPOINT ["docker-entrypoint.sh"].
# MUST be cleared: the Wardyn docker driver sets Cmd=["sleep","infinity"] at
# container create and launches the agent via Exec.  An ENTRYPOINT here would
# wrap the driver-supplied Cmd, causing the sandbox to tear down immediately.
ENTRYPOINT []
CMD []
