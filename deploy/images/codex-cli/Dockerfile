# wardyn/agent-codex-cli: OpenAI Codex CLI coding-agent image.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from the repository root.
# Stage 2: node:22-bookworm-slim installs Codex CLI plus OS utilities.
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
#   Codex CLI reads OPENAI_API_KEY from the environment. Because the Wardyn
#   sandbox env must NOT contain secrets (invariant 1), the recommended path is:
#     1. Create an api_key CredentialGrant for the run with host=api.openai.com.
#     2. The proxy mints the key at startup and injects it via an InjectionRule.
#     3. Configure codex to route through the proxy (HTTP_PROXY env is set by
#        dispatch; Codex honours it for plain HTTP; for HTTPS the CONNECT tunnel
#        passes through and the proxy injects credentials on plain-HTTP legs only).
#   An openai LLM route (/wardyn/llm/openai/<rest>) is FUTURE WORK (v0.5).
#   Until then, operators who want Codex to use brokered credentials must either:
#     a. Use a custom base URL pointing at an operator-controlled relay, OR
#     b. Inject OPENAI_API_KEY via an api_key grant (the operator sets it up and
#        the proxy's InjectionRule for api.openai.com injects it into requests).
#   This image does NOT set OPENAI_API_KEY — the absence of the key causes codex
#   to fail closed if no grant is configured, which is the correct default.

# ── Stage 1: Go builder ───────────────────────────────────────────────────────
FROM golang:1.26@sha256:ae5a2316d12f3e78fd99177dad452e6ad4f240af2d71d57b480c3477f250fec6 AS builder

WORKDIR /build

# Corporate build support (no-ops on an OSS build): GOTOOLCHAIN=local skips the
# pinned-toolchain self-upgrade fetch (blocked behind a TLS-MITM proxy); the
# optional corp CA (host-staged deploy/images/corp-ca.pem, gitignored) is added to
# the system trust so `go mod download` can verify the module proxy through
# corporate MITM. The README.md companion makes the COPY succeed when the
# corp-ca.pem* glob matches nothing.
ENV GOTOOLCHAIN=local
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-rec ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-git-helper ./cmd/wardyn-git-helper \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-scan ./cmd/wardyn-scan

# ── Stage 2: agent runtime ────────────────────────────────────────────────────
FROM node:26-bookworm-slim@sha256:2d49d876e96237d76de412761cf05dbfe5aee325cc4406a4d41d5824c5bb8beb

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        curl \
        ca-certificates \
        asciinema \
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

# Install Codex CLI globally.
# Package name: @openai/codex  (installs the `codex` binary).
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi \
 && if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi \
 && npm install -g @openai/codex

# The node:22-bookworm-slim base image ships a `node` user at uid=1000.
# Rename it to `agent` and relocate the home directory so the sandbox contract
# is satisfied without creating a conflicting second uid-1000 user.
RUN usermod  -l agent node \
 && usermod  -d /home/agent -m agent \
 && groupmod -n agent node \
 && mkdir -p /home/agent/work \
 && chown -R agent:agent /home/agent

# Copy the statically-linked binaries from the build stage.
COPY --from=builder /out/wardyn-rec        /usr/local/bin/wardyn-rec
COPY --from=builder /out/wardyn-git-helper /usr/local/bin/wardyn-git-helper
# wardyn-scan: in-sandbox workspace scanner run by a governed scan run.
COPY --from=builder /out/wardyn-scan       /usr/local/bin/wardyn-scan

# agent-run launcher.
# agent-run-lib.sh: functions shared with every other agent image's agent-run.
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

# The node:22-bookworm-slim base image sets ENTRYPOINT ["docker-entrypoint.sh"].
# MUST be cleared: the Wardyn docker driver sets Cmd=["sleep","infinity"] at
# container create and launches the agent via Exec.  An ENTRYPOINT here would
# wrap the driver-supplied Cmd, causing the sandbox to tear down immediately.
ENTRYPOINT []
CMD []
