# wardyn/agent-base: the Wardyn agent image contract, with NO vendor CLI.
#
# WHY THIS EXISTS. The agent-claude-code image bundles Anthropic's Claude Code
# CLI, which is not open-source software: its package declares
# "SEE LICENSE IN README.md" and neither that file nor a LICENSE ships inside the
# image, so someone pulling it cannot even read the terms they are bound by. That
# is not a thing this project is willing to redistribute from a public registry,
# so agent-claude-code is a LOCAL BUILD RECIPE (`make agent-images`) rather than a
# published artifact, and this image is what gets published in its place.
#
# It carries everything the runner contract needs — wardyn-rec, wardyn-git-helper,
# wardyn-scan, wardyn-toolgate, agent-run, the agent user, the credential helper,
# tmux, asciinema — and no coding agent. Layer your own on top:
#
#     FROM ghcr.io/cjohnstoniv/agent-base:<version>
#     RUN npm install -g <your-agent-cli>@<exact version>
#
# Pin that version exactly, the way every version in this image set is pinned:
# an unversioned global install means two builds of the same git tag ship
# different agents.
#
# You then install the vendor CLI under your OWN agreement with that vendor,
# which is the only arrangement that is actually clean for both of us.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from the repository root.
# Both are pure-Go CGO_ENABLED=0 builds so the binaries are fully static and
# run inside the node:24-trixie-slim stage without any extra libc.
#
# Stage 2: node:24-trixie-slim installs the OS packages needed by wardyn-rec
# (asciinema) and the agent (git, curl, ca-certs). No agent CLI: the
# claude-code image adds it.
#
# IMAGE CONTRACT (all agent images must satisfy these — see deploy/images/README.md):
#   - NO ENTRYPOINT: the Wardyn docker driver sets Cmd=["sleep","infinity"];
#     an ENTRYPOINT would wrap that, tearing the sandbox down immediately.
#   - /usr/local/bin/agent-run "<task>": runs one agent task non-interactively.
#   - /usr/local/bin/wardyn-rec: session recorder (exec asciinema or .log fallback).
#   - /usr/local/bin/wardyn-toolgate: stdio MCP relay that parks claude's tool-use
#     permission prompts on Wardyn approvals (tool_approvals=hold runs only).
#   - /usr/local/bin/wardyn-git-helper: git credential helper for brokered tokens.
#   - USER agent (uid 1000), home /home/agent, workspace /home/agent/work.
#   - System gitconfig wires the credential helper GLOBALLY (all hosts).
#   - ANTHROPIC_BASE_URL points at the proxy's LLM route (api-key default) so
#     Claude Code routes API calls through wardyn-proxy (no direct egress, no
#     secrets in env).  agent-run overrides this per detected auth mode
#     (subscription / bedrock unset it to reach the backend via the HTTPS_PROXY
#     CONNECT tunnel with resident creds).

# ── Stage 1: Go builder ───────────────────────────────────────────────────────
# --platform=$BUILDPLATFORM: runs on the BUILDER's arch and cross-compiles to
# $TARGETARCH below. Every binary here is pure Go (CGO_ENABLED=0), so this stage
# never needs QEMU. Stage 2 does (apt + npm run as the target arch).
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

# Cache module downloads before the full source copy (layer cache friendly).
COPY go.mod go.sum ./
RUN go mod download

# Full source tree needed to build the two binaries.
COPY . .

# TARGETARCH is BuildKit's automatic per-target ARG; empty under the legacy
# builder, and `GOARCH=` empty is how Go spells "use the default".
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-rec ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-git-helper ./cmd/wardyn-git-helper \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-scan ./cmd/wardyn-scan \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-toolgate ./cmd/wardyn-toolgate

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
LABEL org.opencontainers.image.title="agent-base" \
      org.opencontainers.image.description="Wardyn agent base image: the image contract with no vendor CLI" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn" \
      org.opencontainers.image.documentation="https://github.com/cjohnstoniv/wardyn/blob/main/THIRD-PARTY-NOTICES.md"


# asciinema (2.4.0 in trixie) is available as an apt package.
# License hygiene: asciinema is GPL; wardyn-rec only ever *execs* it as a
# subprocess — it is never linked into the binary — so Wardyn stays Apache-2.0.
# If the apt package were ever removed from the trixie repo, wardyn-rec
# gracefully falls back to a plain .log capture (see cmd/wardyn-rec/main.go).
#
# openssh-sftp-server + socat: the SSH-gateway lane (see docs/SSH.md; wired by
# the SSH gateway itself, not built here) needs an in-sandbox SFTP subsystem
# (/usr/lib/openssh/sftp-server) and a general-purpose relay (/usr/bin/socat)
# to bridge a gateway-side connection into the sandbox — see
# deploy/images/README.md's "SSH relay binaries" note for the exact paths.
# tmux + openssh-client already ship here (do not re-touch that pairing/
# known_hosts baking).
# apt-get upgrade: take the current security updates for packages the pinned base already carries.
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
        git \
        curl \
        ca-certificates \
        asciinema \
        tmux \
        openssh-client \
        openssh-sftp-server \
        socat \
        rsync \
        corkscrew \
    && rm -rf /var/lib/apt/lists/*

# ── SSH SCM lane (git-over-SSH clone via SSH-over-443) ────────────────────────
# openssh-client provides `ssh`; corkscrew is the HTTP-CONNECT proxy helper used
# as an ssh ProxyCommand so git-over-SSH tunnels through the wardyn-proxy sidecar
# (the only egress route out of the sandbox) to ssh.github.com:443 /
# ssh.dev.azure.com:443 — SSH-over-443 reuses the CONNECT-443 lane with no
# port-22 egress. agent-run wires GIT_SSH_COMMAND + a ~/.ssh config at clone time
# (only when the run has an ssh_key grant); the private key stays in the run's
# proxy, whose ssh agent signs the clone's SSH sign-in. See GrantSSHKey.
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
      'github.com,ssh.github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=' \
      'ssh.dev.azure.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7Hr1oTWqNqOlzGJOfGJ4NakVyIzf1rXYd4d7wo6jBlkLvCA4odBlL0mDUyZ0/QUfTTqeu+tm22gOsv+VrVTMk6vwRU75gY/y9ut5Mb3bR5BV58dKXyq9A9UeB5Cakehn5Zgm6x1mKoVyf+FFn26iYqXJRgzIZZcZ5V6hrE0Qg39kZm4az48o0AUbf6Sp4SLdvnuMa2sVNwHBboS7EJkm57XQPVU3/QpyNLHbWDdzwtrlS+ez30S3AdYhLKEOxAG8weOnyrtLJAUen9mTkol8oII1edf7mWWbWVf0nBmly21+nZcmCTISQBtdcyPaEno7fFQMDD26/s0lfKob4Kw8H' \
      > /etc/ssh/ssh_known_hosts \
    && chmod 0644 /etc/ssh/ssh_known_hosts \
    # Guard: ssh-keygen exits 0 and silently skips a line it cannot parse, so compare counts.
    && test "$(ssh-keygen -l -f /etc/ssh/ssh_known_hosts | wc -l)" = "$(wc -l < /etc/ssh/ssh_known_hosts)"

# tmux config for the persistent attach session: no status bar (clean terminal,
# no xterm.js line-drawing artifacts), truecolor, tmux-owned mouse wheel, large scrollback.
COPY deploy/images/common/tmux.conf /etc/tmux.conf

# Corporate build support (no-ops on OSS): install the optional corp CA into the
# system + node trust so `npm install -g` can verify registry TLS through a corp
# MITM proxy. The proxy/registry knobs are build-only ARGs (NOT ENV) — a persistent
# proxy ENV would leak into agent RUNS, whose only egress is wardyn-proxy.
#
# NODE_EXTRA_CA_CERTS points at node-extra-ca.pem, NOT corp-ca.pem directly
# (#1114): a Dockerfile ENV can't be conditional, and corp-ca.pem itself only
# exists when a corp CA was staged — pointing NODE_EXTRA_CA_CERTS straight at
# it made Node print "Warning: Ignoring extra certs ... load failed" on every
# start of a default (no-corp-CA) build. node-extra-ca.pem is created in BOTH
# branches below — empty when no CA was staged (Node reads it, finds zero
# certs, stays silent), the corp CA's own bytes when one was (Node trusts it,
# unchanged from before).
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates && \
        cp /corp-ca/corp-ca.pem /corp-ca/node-extra-ca.pem; \
    else \
        : > /corp-ca/node-extra-ca.pem; \
    fi
ENV NODE_EXTRA_CA_CERTS=/corp-ca/node-extra-ca.pem

# No agent CLI is installed in this image. The Claude Code CLI (CLAUDE_INSTALL,
# CLAUDE_CODE_VERSION) is installed by deploy/images/claude-code/Dockerfile, not
# here; the ARGs below are only the proxy and npm settings this image's own npm
# pin and apt/npm steps use.
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
ARG TARGETARCH
# The node base's bundled npm has shipped a vendored node-tar carrying
# CVE-2026-59873 (gzip-bomb DoS) — a CRITICAL the trivy gate fails on, and it
# is npm's INTERNAL copy, so no application-level pin reaches it. Pin npm
# itself instead of trusting whatever the base bundles.
#
# 11.19.1 is the FLOOR, and a higher npm is not automatically a safer one:
# 11.19.0 vendors tar 7.5.19 and 12.0.2 still vendors 7.5.19 — both inside
# CVE-2026-73566's vulnerable range (node-tar <= 7.5.20, patched 7.5.21).
# 11.19.1 vendors 7.5.22. Check what a candidate ACTUALLY vendors before
# bumping (`tar -xzOf npm-<v>.tgz package/node_modules/tar/package.json`),
# rather than assuming the newest release carries the fix. Pinned exactly, and
# bumped deliberately, like every other version here.
ARG NPM_VERSION=11.19.1
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && npm install -g "npm@${NPM_VERSION}" \
 && npm --version

# ── GitHub CLI (gh) ──────────────────────────────────────────────────────────
# The versioned .deb from the cli/cli release, checked against its sha256 per
# architecture (the release's gh_<version>_checksums.txt). A release URL stays
# put, unlike cli.github.com's apt index, which carries only the newest version;
# a bump moves GH_VERSION and both digests together. gh gets no GitHub
# credential here: a run with a GitHub CLI row gets an inert GH_TOKEN, and the
# proxy adds the App token per request.
ARG GH_VERSION=2.102.0
ARG GH_SHA256_AMD64=7e54a307f90afdc59796c325ec0c49fb09e6c18537727207a8ac7513584ea5b0
ARG GH_SHA256_ARM64=5006962696f01e1624b3fcf1f9d8e1a11547f24bf067dd2a0371b7b421945237
ARG TARGETARCH
RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
        amd64) sum="$GH_SHA256_AMD64";; \
        arm64) sum="$GH_SHA256_ARM64";; \
        *) echo "unsupported arch '$arch' for gh" >&2; exit 1;; \
    esac; \
    printf '%s' "$sum" | grep -zExq '[0123456789abcdefABCDEF]{64}'; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${arch}.deb" -o /tmp/gh.deb; \
    echo "$sum  /tmp/gh.deb" | sha256sum --strict -c -; \
    dpkg -i /tmp/gh.deb; \
    rm -f /tmp/gh.deb

# ── AWS CLI v2 (the AWS CLI component, cli.kind aws) ─────────────────────────
# The aws-sso image's verified install, byte for byte from the PGP key to the
# pager setting: scripts/test-image-recipes.py fails when a copy drifts, so a
# version or pin bump is made in every copy at once. unzip unpacks the
# installer and gnupg verifies it; the install purges gnupg again.
RUN apt-get update && apt-get install -y --no-install-recommends unzip gnupg \
    && rm -rf /var/lib/apt/lists/*
# AWS's published PGP public key (Key ID A6310ACC4672475C, fingerprint
# FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C), baked in so the build
# never depends on a keyserver — same convention as the baked GitHub/Azure
# DevOps SSH host keys in deploy/images/claude-code/Dockerfile. Documented at
# https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
# ("Verifying the integrity of your downloaded zip file"). If AWS ever rotates
# this key the verify step below fails CLOSED (build error), never silently.
RUN printf '%s\n' \
      '-----BEGIN PGP PUBLIC KEY BLOCK-----' \
      '' \
      'mQINBF2Cr7UBEADJZHcgusOJl7ENSyumXh85z0TRV0xJorM2B/JL0kHOyigQluUG' \
      'ZMLhENaG0bYatdrKP+3H91lvK050pXwnO/R7fB/FSTouki4ciIx5OuLlnJZIxSzx' \
      'PqGl0mkxImLNbGWoi6Lto0LYxqHN2iQtzlwTVmq9733zd3XfcXrZ3+LblHAgEt5G' \
      'TfNxEKJ8soPLyWmwDH6HWCnjZ/aIQRBTIQ05uVeEoYxSh6wOai7ss/KveoSNBbYz' \
      'gbdzoqI2Y8cgH2nbfgp3DSasaLZEdCSsIsK1u05CinE7k2qZ7KgKAUIcT/cR/grk' \
      'C6VwsnDU0OUCideXcQ8WeHutqvgZH1JgKDbznoIzeQHJD238GEu+eKhRHcz8/jeG' \
      '94zkcgJOz3KbZGYMiTh277Fvj9zzvZsbMBCedV1BTg3TqgvdX4bdkhf5cH+7NtWO' \
      'lrFj6UwAsGukBTAOxC0l/dnSmZhJ7Z1KmEWilro/gOrjtOxqRQutlIqG22TaqoPG' \
      'fYVN+en3Zwbt97kcgZDwqbuykNt64oZWc4XKCa3mprEGC3IbJTBFqglXmZ7l9ywG' \
      'EEUJYOlb2XrSuPWml39beWdKM8kzr1OjnlOm6+lpTRCBfo0wa9F8YZRhHPAkwKkX' \
      'XDeOGpWRj4ohOx0d2GWkyV5xyN14p2tQOCdOODmz80yUTgRpPVQUtOEhXQARAQAB' \
      'tCFBV1MgQ0xJIFRlYW0gPGF3cy1jbGlAYW1hem9uLmNvbT6JAlQEEwEIAD4CGwMF' \
      'CwkIBwIGFQoJCAsCBBYCAwECHgECF4AWIQT7Xbd/1cEYuAURraimMQrMRnJHXAUC' \
      'akV0ygUJDqP4lQAKCRCmMQrMRnJHXFHjD/9eyZLYcKuQOlLvtqSDtUBiEZf6ZZjM' \
      'i3ygYH8rJNtuToUH+HvSpe819urJCquXhDrlK6N+aqW0hCLtNABJG/vsafIgvIYJ' \
      'hSGgpgtNnQyMV1jViRWqPjbouw8OkYKBThUfT1i2Y+wn58ifs6ODBCmTexWtXspA' \
      'Si+Gt49xDOW0APmbOPnI+a4HJW6tVEo6MWS0WjzpiBayR3d1A4pt4YrPfSdDgpLo' \
      'h2SLQqlRqvvVZJaWBjhkErNFpfsBA06sDcPEOb0G8LBUbR4WOcdvhe5LubJbZuxC' \
      'AG9kNPCVeQP1ixwjgjXKysaxeQ6rv0VzIQgRp6tLVLWhy6AKDNvLjFSsmXZ1Wl08' \
      'Y/RlOHXlzLuQMRE6sR1wOdRxc9TsrNWTGiBK65cvSWOy03JeBkQQ8pesqltiyxI9' \
      'U21kkgiXtTSKNGfKK8pO27D81YANhRqPK7iTp6kuFiY2WtOg90KTMNlIT+Ff85Y2' \
      'b1rHj6Z0SrCkJujhWk3IBPic/wJgz01LEc/OAdUPlby90RJZcIBhSlWhT7mXnXIO' \
      'c0HWlNQrns2s3CTyYwZSiSlYe9ApeLwhjDo8NhbFuCAy61l6O5UsR4AfZxx/rGKv' \
      '2wFb1/RN/P4gNe6vmxZAPjR0AQcwD3tc2McimOLr/22kmPz8IH3I0X7WoSFr0Biz' \
      'E91G7bb0hOb/cA==' \
      '=knv7' \
      '-----END PGP PUBLIC KEY BLOCK-----' \
      > /aws-cli-pubkey.asc

# Install AWS CLI v2. There is no apt package for v2 (only apt's v1, which
# does not support sso-session/device-code) — DEFAULT: download the official
# per-version zip installer and verify it against the baked AWS public key
# above (fails the build closed on a signature mismatch). Behind a strict
# allowlist mirror that can't reach awscli.amazonaws.com, set
# AWS_CLI_INSTALL=staged to consume a host-staged installer zip instead — stage
# it as deploy/images/aws-sso/awscliv2-bin (gitignored) on a host that CAN
# reach the download, e.g.:
#   curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64-<ver>.zip -o deploy/images/aws-sso/awscliv2-bin
# then build with: make agent-images AWS_CLI_INSTALL=staged
ARG AWS_CLI_INSTALL=download
ARG AWS_CLI_VERSION=2.37.1
ARG AWS_CLI_SHA256=
ARG AWS_CLI_SIG_SHA256=
ARG TARGETARCH
# Host-staged installer zip (opt-in, gitignored). The README.md decoy makes
# the COPY succeed when the awscliv2-bin* glob matches nothing (the download
# default path) — same trick as claude-code's claude-bin* / codex-bin* globs.
COPY deploy/images/README.md deploy/images/aws-sso/awscliv2-bin* /awscli-stage/
RUN set -eu; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
        amd64) plat=x86_64; zip_sum=61a79950dfb436985611a258d3a8f85a47e13498fcc72c7d8d0597f5c9fde133; sig_sum=6b37863f95a59bfd9ad73e91e618ed73ca48e4f1b7b8046337e23d3561fc786f;; \
        arm64) plat=aarch64; zip_sum=30642dc4840012614229feb68314637d024500f565d92f513a422bdd55844a90; sig_sum=82e029c0b74763507a0f95dbddb2a57b28251902e571fe7622f15a8dbf6ce4ab;; \
        *) echo "unsupported arch '${TARGETARCH:-unknown}' for AWS CLI" >&2; exit 1;; \
    esac; \
    if [ "$AWS_CLI_INSTALL" = "staged" ]; then \
        [ -f /awscli-stage/awscliv2-bin ] || { echo "AWS_CLI_INSTALL=staged but deploy/images/aws-sso/awscliv2-bin is not staged" >&2; exit 1; }; \
        cp /awscli-stage/awscliv2-bin /tmp/awscliv2.zip; \
        echo "using host-staged AWS CLI installer zip (unverified — trust the staging host)"; \
    else \
        if [ "$AWS_CLI_VERSION" != "2.37.1" ]; then \
            [ -n "$AWS_CLI_SHA256" ] && [ -n "$AWS_CLI_SIG_SHA256" ] || { echo "AWS_CLI_VERSION=$AWS_CLI_VERSION requires AWS_CLI_SHA256 and AWS_CLI_SIG_SHA256 for $plat" >&2; exit 1; }; \
        fi; \
        zip_sum="${AWS_CLI_SHA256:-$zip_sum}"; \
        sig_sum="${AWS_CLI_SIG_SHA256:-$sig_sum}"; \
        printf '%s' "$zip_sum" | grep -zExq '[0123456789abcdefABCDEF]{64}'; \
        printf '%s' "$sig_sum" | grep -zExq '[0123456789abcdefABCDEF]{64}'; \
        base="https://awscli.amazonaws.com"; \
        url="${base}/awscli-exe-linux-${plat}-${AWS_CLI_VERSION}.zip"; \
        curl -fsSL "$url" -o /tmp/awscliv2.zip; \
        curl -fsSL "${url}.sig" -o /tmp/awscliv2.sig; \
        echo "$zip_sum  /tmp/awscliv2.zip" | sha256sum --strict -c -; \
        echo "$sig_sum  /tmp/awscliv2.sig" | sha256sum --strict -c -; \
        gpg --batch --no-tty --import /aws-cli-pubkey.asc; \
        gpg --batch --no-tty --verify /tmp/awscliv2.sig /tmp/awscliv2.zip; \
        echo "downloaded + checksum/GPG-verified AWS CLI ${AWS_CLI_VERSION} (${plat})"; \
    fi; \
    mkdir -p /tmp/awscli-install; \
    unzip -q /tmp/awscliv2.zip -d /tmp/awscli-install; \
    /tmp/awscli-install/aws/install; \
    # The zip carries AWS's bundled third-party attributions at
    # aws/THIRD_PARTY_LICENSES, but `aws/install` copies only dist/ — so the
    # file would die with the rm below and the shipped image would convey the
    # bundled components with no attribution at all (Apache-2.0 s4). Preserve
    # it explicitly; `install -D` fails the build closed if the zip layout
    # ever stops carrying it.
    install -D -m 0644 /tmp/awscli-install/aws/THIRD_PARTY_LICENSES /usr/share/doc/aws-cli/THIRD_PARTY_LICENSES; \
    rm -rf /tmp/awscliv2.zip /tmp/awscliv2.sig /tmp/awscli-install /aws-cli-pubkey.asc /awscli-stage; \
    apt-get purge -y --auto-remove gnupg; \
    rm -rf /var/lib/apt/lists/*

# Never let the CLI's pager block a non-tty exec (agent-run / a scripted
# selftest have no terminal to page through); an interactive `wardyn run attach`
# shell can still `less` things manually.
ENV AWS_PAGER=""

# /home/agent/drive is the RESERVED mount point for a user drive (runner.DriveTarget).
# It is created here, owned by agent, EVEN THOUGH nothing mounts it on most runs:
# Docker's copy-up gives a fresh named volume the uid/gid of the image directory it
# is mounted over, so a MANAGED drive's first run lands on storage uid 1000 can
# write. Without this the volume's root would be root-owned and every managed drive
# would be read-only in practice — and fixing that at run time would mean wardynd
# chowning host/volume state, which it must never do.
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
# wardyn-toolgate: parks tool-use approvals on the Wardyn UI (hold runs).
COPY --from=builder /out/wardyn-toolgate   /usr/local/bin/wardyn-toolgate
# wardyn: the in-sandbox `wardyn needs-you <reason>` ("Needs you" takeover request), run by wardyn-toolgate.
COPY deploy/images/common/wardyn-in-sandbox /usr/local/bin/wardyn

# agent-run: the thin shell wrapper used by the Wardyn driver to launch one task.
# agent-run-lib.sh: functions shared with every other agent image's agent-run.
COPY deploy/images/common/agent-run-lib.sh /usr/local/bin/agent-run-lib.sh
# agent-base ships no coding agent, so it cannot implement agent-run. The stub
# fails with an explanation rather than leaving "command not found" to debug;
# a derived image replaces it.
COPY deploy/images/common/agent-run-stub /usr/local/bin/agent-run
# The attach shell (interactive bash) sources this: it waits for the workspace to
# finish preparing (repo clone) and cd's into it, so an attach that lands mid-clone
# never shows an empty ~/work. See deploy/images/common/attach-bashrc.sh.
COPY --chown=agent:agent deploy/images/common/attach-bashrc.sh /home/agent/.bashrc
RUN chmod +x /usr/local/bin/wardyn-rec \
           /usr/local/bin/wardyn-git-helper \
           /usr/local/bin/wardyn-scan \
           /usr/local/bin/wardyn-toolgate \
           /usr/local/bin/wardyn \
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
# No provider base URL is preset: that is the derived image's choice.

# ── What a Claude Code CLI fetches on its OWN behalf, OFF ─────────────────────
# INERT IN THIS IMAGE, AND HERE ON PURPOSE. agent-base ships no coding agent, so
# nothing reads these three vars as built — but this is the image the PUBLISHED
# claude-code lane actually is: install.sh maps the `claude-code` harness key to
# ghcr.io/…/agent-base, and the BYOI recipe at the top of this file is
# `FROM agent-base` + your own CLI. agent-claude-code is a local build recipe
# (0.6.2, licensing) published nowhere, so its Dockerfile's copy of this block
# reaches nobody who installs Wardyn the documented way.
#
# The DEFAULT interactive run is the path that needs IMAGE env specifically: with
# no seed, `claude` is started by attach-bashrc.sh in a fresh attach exec that is
# not a descendant of agent-run, so agent-run-lib.sh's exports cannot reach it.
# Only ENV a derived image inherits does.
#
# What each one removes, measured host-by-host through a real proxy,
# because the obvious attribution was
# wrong once already:
#   - CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL — the reported pair.
#     On first REPL start the CLI auto-installs the official PLUGIN MARKETPLACE
#     from downloads.claude.ai, git-falling-back to github.com. Beyond the
#     first-use approvals it parks: a governed, version-pinned image must not
#     fetch and install third-party code at start.
#   - DISABLE_AUTOUPDATER — NOT what removes downloads.claude.ai (on an npm
#     install the update check dials registry.npmjs.org, which the default policy
#     already allows). Off anyway: a pinned sandbox must not upgrade itself
#     mid-run. A human's `claude update` still works.
#   - CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC — the raw.githubusercontent.com
#     changelog fetch, plus telemetry / error intake.
#
# A derived image that installs a DIFFERENT agent is unaffected: three unread
# strings in its environment. One that installs Claude Code inherits the quiet
# boot without having to know this list exists — which is the point, and is
# written down as a contract in deploy/images/README.md.
ENV DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1

WORKDIR /home/agent/work

USER agent

# The node:24-trixie-slim base image sets ENTRYPOINT ["docker-entrypoint.sh"].
# MUST be cleared: the Wardyn docker driver sets Cmd=["sleep","infinity"] at
# container create and the agent process is launched later via Exec.  An
# ENTRYPOINT would wrap the driver-supplied Cmd as arguments, causing the
# entrypoint to exit and tear the sandbox down immediately.
ENTRYPOINT []
CMD []
