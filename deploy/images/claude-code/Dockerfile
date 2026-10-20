# wardyn/agent-claude-code: Claude Code coding-agent image.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from the repository root.
# Both are pure-Go CGO_ENABLED=0 builds so the binaries are fully static and
# run inside the node:24-trixie-slim stage without any extra libc.
#
# Stage 2: node:24-trixie-slim installs the Claude Code CLI plus the OS
# packages needed by wardyn-rec (asciinema) and the agent (git, curl, ca-certs).
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
LABEL org.opencontainers.image.title="agent-claude-code" \
      org.opencontainers.image.description="Wardyn agent image: Claude Code" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND LicenseRef-Anthropic-Terms" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn" \
      org.opencontainers.image.documentation="https://github.com/cjohnstoniv/wardyn/blob/main/THIRD-PARTY-NOTICES.md"


# asciinema (2.4.0-1 in trixie) is available as an apt package.
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
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
# NODE_EXTRA_CA_CERTS points at node-extra-ca.pem, NOT corp-ca.pem (#1114, as in the base
# image): corp-ca.pem exists only when a corp CA was staged, and a dangling path makes Node
# print "Ignoring extra certs" on every start of a default build. node-extra-ca.pem exists in
# both branches: empty without a CA, the CA's bytes with one.
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates && \
        cp /corp-ca/corp-ca.pem /corp-ca/node-extra-ca.pem; \
    else \
        : > /corp-ca/node-extra-ca.pem; \
    fi
ENV NODE_EXTRA_CA_CERTS=/corp-ca/node-extra-ca.pem

# Install the Claude Code CLI. DEFAULT: npm (@anthropic-ai/claude-code, installs
# the `claude` binary). Behind a corp proxy where public npm is blocked, set
# CLAUDE_INSTALL=native to install the native `claude` binary instead — from a
# host-staged file if present (fully offline / strict allowlist mirror; stage it
# with scripts/stage-agent-binary.sh), else downloaded checksum-verified from the
# official downloads.claude.ai surface (the ONLY host that path needs reachable
# through the corp proxy). See docs/adoption/corp-image-authoring.md "Native-binary agent installs".
#
# CLAUDE_CODE_VERSION is a PINNED exact version, not a channel — a released
# agent image must be reproducible, and a floating `stable`/`latest` means two
# builds of the same git tag ship different agents (D3, supply chain). The SAME
# value drives BOTH install paths: npm takes it as the version specifier, and
# the native download requires an exact release and a pinned manifest. Channel
# overrides remain available through npm or a host-staged native binary.
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
ARG CLAUDE_INSTALL=npm
ARG CLAUDE_CODE_VERSION=2.1.231
ARG CLAUDE_MANIFEST_SHA256=
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
# Host-staged native binary (opt-in, gitignored). The README.md decoy makes the
# COPY succeed when the corp-bin glob matches nothing (the npm default path).
# A staged binary built for the other arch (see stage-agent-binary.sh --arch) is refused below by its ELF
# machine type (byte 18: 0x3e amd64, 0xb7 arm64), else it ships and fails only when the image starts.
# Anything else (not ELF, another machine type) installs as before.
COPY deploy/images/README.md deploy/images/claude-code/claude-bin* /claude-stage/
RUN set -eu; \
    if [ "$CLAUDE_INSTALL" = "native" ]; then \
        if [ -f /claude-stage/claude-bin ]; then \
            want="${TARGETARCH:-$(dpkg --print-architecture)}"; \
            case "$(od -An -tx1 -j18 -N1 /claude-stage/claude-bin | tr -d ' ')" in 3e) have=amd64;; b7) have=arm64;; *) have=unknown;; esac; \
            [ "$have" = unknown ] || [ "$have" = "$want" ] || { echo "staged claude-bin is $have but this image builds for $want; restage with: scripts/stage-agent-binary.sh claude-code <version> --arch $want" >&2; exit 1; }; \
            install -m 0755 /claude-stage/claude-bin /usr/local/bin/claude; \
            echo "installed host-staged native claude"; \
        else \
            case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
                amd64) plat=linux-x64;; \
                arm64) plat=linux-arm64;; \
                *) echo "unsupported arch '${TARGETARCH:-unknown}' for native claude" >&2; exit 1;; \
            esac; \
            base="https://downloads.claude.ai/claude-code-releases"; \
            ver="$CLAUDE_CODE_VERSION"; \
            case "$ver" in stable|latest) echo "native download requires an exact CLAUDE_CODE_VERSION and CLAUDE_MANIFEST_SHA256; stage a binary or use npm for channels" >&2; exit 1;; esac; \
            manifest_sum="$CLAUDE_MANIFEST_SHA256"; \
            if [ -z "$manifest_sum" ] && [ "$ver" = "2.1.231" ]; then \
                manifest_sum=670096acfde2def2e5b71178908e40983bf6e3cb6155300ee4a669a5768d3e43; \
            fi; \
            [ -n "$manifest_sum" ] || { echo "native download of $ver requires CLAUDE_MANIFEST_SHA256" >&2; exit 1; }; \
            printf '%s' "$manifest_sum" | grep -zExq '[0123456789abcdefABCDEF]{64}'; \
            curl -fsSL "$base/$ver/manifest.json" -o /tmp/claude-manifest.json; \
            echo "$manifest_sum  /tmp/claude-manifest.json" | sha256sum --strict -c -; \
            sum="$(grep -A3 "\"$plat\"" /tmp/claude-manifest.json | grep -oiE '[a-f0-9]{64}' | head -1)"; \
            [ -n "$sum" ] || { echo "no sha256 for $plat in $ver manifest" >&2; exit 1; }; \
            printf '%s' "$sum" | grep -zExq '[0123456789abcdefABCDEF]{64}'; \
            curl -fsSL "$base/$ver/$plat/claude" -o /usr/local/bin/claude; \
            echo "$sum  /usr/local/bin/claude" | sha256sum --strict -c -; \
            chmod 0755 /usr/local/bin/claude; \
            rm -f /tmp/claude-manifest.json; \
            echo "installed native claude $ver ($plat), checksum-verified"; \
        fi; \
    else \
        if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi; \
        if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi; \
        if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi; \
        npm install -g "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"; \
        npm config delete proxy https-proxy; \
        echo "installed claude $CLAUDE_CODE_VERSION via npm"; \
    fi

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

# The node:24-trixie-slim base image ships a `node` user at uid=1000.
# Rename it to `agent` and relocate the home directory to /home/agent so the
# sandbox contract (USER agent, uid 1000, home /home/agent, work /home/agent/work)
# is satisfied without creating a second uid-1000 user (which useradd rejects).
# /home/agent/drive is the RESERVED user-drive mount point (runner.DriveTarget),
# created here owned by agent for the copy-up reason deploy/images/base/Dockerfile
# states in full: a fresh managed volume inherits the uid/gid of the image directory
# it is mounted over, so without it a docker_volume drive's root is root-owned and a
# writable managed drive is read-only in practice for uid 1000.
RUN usermod  -l agent node \
 && usermod  -d /home/agent -m agent \
 && groupmod -n agent node \
 && mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent \
 # Debian 13's home is 0700 (Debian 12: 0755); a run under another uid must reach it.
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
COPY deploy/images/claude-code/agent-run /usr/local/bin/agent-run
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
ENV ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic

# Everything the CLI reaches for on ITS OWN behalf, OFF for every process in this
# image. Against the shipped default policy each of these parks a first-use
# approval before the operator has asked the agent for anything — a new member's
# first impression of the governance model being a prompt about a host they did
# not choose. Delete the traffic rather than widen the policy.
#
# CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1 is the one that removes
# the reported pair. On first REPL start the CLI auto-installs the OFFICIAL
# PLUGIN MARKETPLACE from
# https://downloads.claude.ai/claude-code-releases/plugins/claude-plugins-official,
# falling back to a git clone from github.com — measured as exactly the two hosts
# the field report saw park. A governed, version-pinned image must not fetch and
# install a plugin marketplace at start: that is third-party code entering the
# sandbox outside the artifact the operator scanned and pinned, which is a
# supply-chain decision, not a convenience.
#
# DISABLE_AUTOUPDATER=1 is NOT what removes downloads.claude.ai. On this image's
# npm install (CLAUDE_INSTALL=npm, pinned CLAUDE_CODE_VERSION) the updater's
# startup check dials registry.npmjs.org — twice — which the default policy
# already allows, so it never parked. It is disabled anyway because a
# version-pinned image must not self-update mid-run; `claude update` still works
# when a human asks for it. CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 removes
# the changelog fetch from raw.githubusercontent.com and the telemetry/error
# intake. All three measured host-by-host through a real proxy.
#
# agent-run-lib.sh exports the same three vars with `:-` defaults, and that is
# NOT a duplicate of this ENV — nor a substitute for it. On the DEFAULT
# interactive run there is no seed, so `claude` is started by attach-bashrc.sh in
# a fresh attach exec that is not a descendant of agent-run: the library export
# cannot reach it and only this ENV does. The library export covers task mode and
# the boot pane — i.e. a BYOI/corp image that COPYs agent-run but not this
# Dockerfile keeps those two paths and MUST set these itself for interactive runs.
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
