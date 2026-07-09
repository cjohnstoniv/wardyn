# wardyn/agent-claude-code: Claude Code coding-agent image.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from the repository root.
# Both are pure-Go CGO_ENABLED=0 builds so the binaries are fully static and
# run inside the node:22-bookworm-slim stage without any extra libc.
#
# Stage 2: node:22-bookworm-slim installs the Claude Code CLI plus the OS
# packages needed by wardyn-rec (asciinema) and the agent (git, curl, ca-certs).
#
# IMAGE CONTRACT (all agent images must satisfy these — see deploy/images/README.md):
#   - NO ENTRYPOINT: the Wardyn docker driver sets Cmd=["sleep","infinity"];
#     an ENTRYPOINT would wrap that, tearing the sandbox down immediately.
#   - /usr/local/bin/agent-run "<task>": runs one agent task non-interactively.
#   - /usr/local/bin/wardyn-rec: session recorder (exec asciinema or .log fallback).
#   - /usr/local/bin/wardyn-git-helper: git credential helper for brokered tokens.
#   - USER agent (uid 1000), home /home/agent, workspace /home/agent/work.
#   - System gitconfig wires the credential helper GLOBALLY (all hosts).
#   - ANTHROPIC_BASE_URL points at the proxy's LLM route (api-key default) so
#     Claude Code routes API calls through wardyn-proxy (no direct egress, no
#     secrets in env).  agent-run overrides this per detected auth mode
#     (subscription / bedrock unset it to reach the backend via the HTTPS_PROXY
#     CONNECT tunnel with resident creds).

# ── Stage 1: Go builder ───────────────────────────────────────────────────────
FROM golang:1.26 AS builder

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

# Cache module downloads before the full source copy (layer cache friendly).
COPY go.mod go.sum ./
RUN go mod download

# Full source tree needed to build the two binaries.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-rec ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-git-helper ./cmd/wardyn-git-helper \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-scan ./cmd/wardyn-scan \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/wardyn-verify ./cmd/wardyn-verify

# ── Stage 2: agent runtime ────────────────────────────────────────────────────
FROM node:22-bookworm-slim

# asciinema (2.2.0-1 in bookworm) is available as an apt package.
# License hygiene: asciinema is GPL; wardyn-rec only ever *execs* it as a
# subprocess — it is never linked into the binary — so Wardyn stays Apache-2.0.
# If the apt package were ever removed from the bookworm repo, wardyn-rec
# gracefully falls back to a plain .log capture (see cmd/wardyn-rec/main.go).
RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        curl \
        ca-certificates \
        asciinema \
        tmux \
        openssh-client \
        corkscrew \
    && rm -rf /var/lib/apt/lists/*

# ── SSH SCM lane (git-over-SSH clone via SSH-over-443) ────────────────────────
# openssh-client provides `ssh`; corkscrew is the HTTP-CONNECT proxy helper used
# as an ssh ProxyCommand so git-over-SSH tunnels through the wardyn-proxy sidecar
# (the only egress route out of the sandbox) to ssh.github.com:443 /
# ssh.dev.azure.com:443 — SSH-over-443 reuses the CONNECT-443 lane with no
# port-22 egress. agent-run wires GIT_SSH_COMMAND + a ~/.ssh config at clone time
# (only when the run has an ssh_key grant); the private key is minted from the
# broker, written 0400, and WIPED right after the clone. See GrantSSHKey.
#
# Bake the PUBLISHED host keys so StrictHostKeyChecking=yes verifies the server
# without a TOFU prompt (the sandbox has no interactive tty). These are GitHub's
# and Azure DevOps's official, stable published SSH host keys as of the image
# build date; ssh.github.com presents the SAME keys as github.com. If a provider
# rotates a key, the clone fails CLOSED (host-key mismatch, never silent trust) —
# operators can override per-run via the ssh_key grant's known_hosts_secret_ref.
# 0644 root-owned (public data; the agent uid must read it, cannot rewrite it).
RUN printf '%s\n' \
      'github.com,ssh.github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' \
      'github.com,ssh.github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=' \
      'github.com,ssh.github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4pGCyDXvXWEQvAKG9NkYAB2yKN8x7q0V/HZ3q+3+SbYD8n27/N5T6nKk+lp7Trmwr7dlrGWA+dfj3+ZzZ4yZ9Nfxx6RJ8=' \
      'ssh.dev.azure.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7Hr1oTWqNqOlzGJOfGJ4NakVyIzf1rXYd4d7wo6jBlkLvCA4odBlL0mDUyZ0/QUfTTqeu+tm22gOsv+VrVTMk6vwRU75gY/y9ut5Mb3bR5BV58dKXyq9A9UeB5Cakehn5Zgm6x1mKoVyf+FFn26iYqXJRgzIZZcZ5V6hrE0Qg39kZm4az48o0AUbf6Sp4SLdvnuMa2sVNwHBboS7EJkm57XQPVU3/QpyNLHbWDdzwtrlS+ez30S3AdYhLKEOxAG8weOnyrtLJAUen9mTkol8oII1edf7mWWbWVf0nBmly21+nZcmCTISQBtdcyPaEno7fFQMDD26/s0lfKob4Kw8H' \
      > /etc/ssh/ssh_known_hosts \
    && chmod 0644 /etc/ssh/ssh_known_hosts

# tmux config for the persistent attach session: no status bar (clean terminal,
# no xterm.js line-drawing artifacts), truecolor, mouse, large scrollback.
COPY deploy/images/claude-code/tmux.conf /etc/tmux.conf

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

# Install Claude Code CLI globally.
# Package name: @anthropic-ai/claude-code  (installs the `claude` binary).
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi \
 && if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi \
 && npm install -g @anthropic-ai/claude-code

# The node:22-bookworm-slim base image ships a `node` user at uid=1000.
# Rename it to `agent` and relocate the home directory to /home/agent so the
# sandbox contract (USER agent, uid 1000, home /home/agent, work /home/agent/work)
# is satisfied without creating a second uid-1000 user (which useradd rejects).
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
COPY --from=builder /out/wardyn-verify     /usr/local/bin/wardyn-verify

# agent-run: the thin shell wrapper used by the Wardyn driver to launch one task.
# agent-run-lib.sh: functions shared with every other agent image's agent-run.
COPY deploy/images/common/agent-run-lib.sh /usr/local/bin/agent-run-lib.sh
COPY deploy/images/claude-code/agent-run /usr/local/bin/agent-run
# The attach shell (interactive bash) sources this: it waits for the workspace to
# finish preparing (repo clone) and cd's into it, so an attach that lands mid-clone
# never shows an empty ~/work. See deploy/images/common/attach-bashrc.sh.
COPY --chown=agent:agent deploy/images/common/attach-bashrc.sh /home/agent/.bashrc
RUN chmod +x /usr/local/bin/wardyn-rec \
           /usr/local/bin/wardyn-git-helper \
           /usr/local/bin/wardyn-scan \
           /usr/local/bin/agent-run

# System-wide GLOBAL git credential helper (all hosts).
# wardyn-git-helper speaks the git credential protocol: on `get` it calls
# POST /wardyn/v1/credentials/mint (via the proxy) to obtain a brokered
# credential and emits it for git to consume.  The credential never appears in
# the sandbox env.  It brokers github.com (WARDYN_GITHUB_GRANT_ID) AND matched
# non-GitHub hosts (WARDYN_GIT_PAT_GRANTS, e.g. Azure DevOps / GitLab); for ANY
# other host it emits nothing and git falls through, so a GLOBAL helper is safe.
#
# --secret-file wires the per-run CALLER-AUTH gate: before emitting a credential
# the helper requires the caller to present WARDYN_GIT_HELPER_SECRET matching
# this 0400 agent-owned file (provisioned by agent-run at task time).  This
# config is in the ROOT-owned system gitconfig so the agent cannot rewrite the
# path.  When the file is absent (e.g. an interactive run that never ran
# agent-run) the helper falls open and mints, so legitimate git is never blocked.
RUN git config --system credential.helper \
        '/usr/local/bin/wardyn-git-helper --secret-file /home/agent/.wardyn/git-helper.secret'

# Point Claude Code at the proxy's LLM route — this is the API-KEY default.
# The proxy intercepts plain-HTTP requests to this prefix and forwards them to
# https://api.anthropic.com, injecting the brokered api_key credential.
# Claude Code honours ANTHROPIC_BASE_URL to override the API base; no API key
# is present in the sandbox — the proxy holds and injects it.
# NOTE: agent-run detects the auth mode at task time and OVERRIDES this per run:
# in subscription (resident ~/.claude OAuth creds) and bedrock
# (CLAUDE_CODE_USE_BEDROCK) modes it UNSETs ANTHROPIC_BASE_URL so claude reaches
# the real backend through the HTTPS_PROXY CONNECT tunnel.  This ENV is the
# fallback only for api-key mode.
ENV ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic

WORKDIR /home/agent/work

USER agent

# The node:22-bookworm-slim base image sets ENTRYPOINT ["docker-entrypoint.sh"].
# MUST be cleared: the Wardyn docker driver sets Cmd=["sleep","infinity"] at
# container create and the agent process is launched later via Exec.  An
# ENTRYPOINT would wrap the driver-supplied Cmd as arguments, causing the
# entrypoint to exit and tear the sandbox down immediately.
ENTRYPOINT []
CMD []
