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
#   and OPENAI_API_KEY are still set, unused by codex and deprecated (removal in a later
#   release); only the pinned codex is supported.
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
        rsync \
    && rm -rf /var/lib/apt/lists/*

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
