# wardyn/agent-aws-sso: AWS SSO device-code login image.
#
# This is NOT a coding agent — it has no LLM harness. Its ONLY job is to host
# an interactive `aws sso login --sso-session wardyn --no-browser --use-device-code` session so an
# operator can `wardyn attach` into a governed sandbox and authenticate an AWS
# SSO profile. It carries AWS CLI v2 — v1 (apt's `awscli` package) does not
# support the sso-session / device-code flow, so this image does NOT use apt
# for the CLI itself.
#
# Stage 1: build wardyn-rec and wardyn-git-helper from
# the repository root — pure-Go CGO_ENABLED=0 builds, fully static, so the
# runtime stage needs no extra libc for them.
# Stage 2: debian:bookworm-slim (no Node.js — this image never runs npm/node,
# unlike claude-code/codex-cli) installs AWS CLI v2 from the official zip
# installer, GPG-signature-verified against AWS's published public key, plus
# the small toolset the login flow needs.
#
# IMAGE CONTRACT (all agent images must satisfy these — see deploy/images/README.md):
#   - NO ENTRYPOINT: both drivers REPLACE this image's CMD at create time — with
#     `agent-run --idle` for an interactive run, with the shared idle script
#     otherwise (the `idleCmd` variable in the docker driver's CreateSandbox and
#     in its k8s sibling). An ENTRYPOINT would wrap whichever one they chose,
#     tearing the sandbox down immediately. The baked CMD below is therefore
#     never what a Wardyn-launched sandbox runs; it only covers a bare
#     `docker run` of this image.
#   - /usr/local/bin/agent-run "<task>" / --idle / --selftest: same CLI
#     surface as the other images. There is no LLM here, so normal task mode
#     and idle (attach) mode both converge on the same interactive SSO login —
#     the task text is not a prompt, it is ignored (logged, not consumed).
#   - /usr/local/bin/wardyn-rec: session recorder (records the login session).
#   - /usr/local/bin/wardyn-git-helper: backs the system credential helper below.
#   - USER agent (uid 1000), home /home/agent, workspace /home/agent/work.
#   - System gitconfig wires the credential helper GLOBALLY (all hosts).
#
# AWS_CLI_VERSION is pinned (not "latest") so a rebuild is reproducible; bump it
# deliberately. AWS_CLI_INSTALL=staged consumes a host-staged installer zip
# (deploy/images/aws-sso/awscliv2-bin, gitignored) instead of downloading, for
# offline / strict-allowlist builds — mirrors claude-code's CLAUDE_INSTALL
# native/staged-override pattern (see deploy/images/README.md).

# ── Stage 1: Go builder ───────────────────────────────────────────────────────
# --platform=$BUILDPLATFORM: runs on the BUILDER's arch and cross-compiles to
# $TARGETARCH below (pure Go, CGO_ENABLED=0) — no QEMU for the compile. Stage 2
# does need it (apt + the AWS CLI installer run as the target arch; that stage
# already selects x86_64/aarch64 off its own TARGETARCH).
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS builder

WORKDIR /build

# Corporate build support (no-ops on an OSS build): see deploy/images/claude-code/Dockerfile
# for the full rationale — GOTOOLCHAIN=local skips the pinned-toolchain
# self-upgrade fetch; the optional host-staged corp CA is trusted so `go mod
# download` can verify the module proxy through a corporate MITM.
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
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-rec        ./cmd/wardyn-rec \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-git-helper ./cmd/wardyn-git-helper \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/wardyn-aws-sso    ./cmd/wardyn-aws-sso

# ── Stage 2: agent runtime ────────────────────────────────────────────────────
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

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
LABEL org.opencontainers.image.title="agent-aws-sso" \
      org.opencontainers.image.description="Wardyn agent image: AWS SSO" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn" \
      org.opencontainers.image.documentation="https://github.com/cjohnstoniv/wardyn/blob/main/THIRD-PARTY-NOTICES.md"


# ca-certificates/curl: TLS + the CLI's own HTTPS calls (routed via wardyn-proxy).
# unzip: unpacks the AWS CLI installer zip at build time (kept at runtime too —
#   cheap, and handy for an attached operator).
# less: the AWS CLI's default pager; AWS_PAGER="" below disables it anyway so a
#   non-tty exec never hangs, but `less` stays installed for an interactive
#   `wardyn attach` shell where a human may want normal pager behavior.
# git + openssh-client: parity with the sibling images' git-broker wiring.
# gnupg: ONLY needed to verify the AWS CLI installer's GPG signature at build
#   time — purged below so it doesn't bloat the shipped image.
# tmux: the persistent attach session, same pairing the sibling images ship. Here
#   it is load-bearing rather than cosmetic: agent-run --idle starts the sign-in
#   in a `wardyn` session and BOTH drivers attach with `tmux new-session -A -s
#   wardyn bash`, so every attach path joins the sign-in already running instead
#   of getting a bare prompt. Without tmux each attach falls through to `bash -i`
#   and the box is the bare shell finding 4 reported.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        unzip \
        less \
        git \
        openssh-client \
        tmux \
        gnupg \
    && rm -rf /var/lib/apt/lists/*

# Corporate build support (no-ops on OSS): install the optional corp CA into
# the system trust so the AWS CLI download below (and any future apt/curl use)
# can verify TLS through a corp MITM proxy. Build-only ARGs, never persistent
# ENV — see deploy/images/README.md "Corporate-build convention".
COPY deploy/images/README.md deploy/images/corp-ca.pem* /corp-ca/
RUN if [ -f /corp-ca/corp-ca.pem ]; then \
        mkdir -p /usr/local/share/ca-certificates && \
        cp /corp-ca/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt && \
        update-ca-certificates; \
    fi
# Point the AWS CLI at the OS trust store. It ships its OWN Python and its OWN
# bundled CA store, so the update-ca-certificates above — and every var the
# siblings set (NODE_EXTRA_CA_CERTS et al) — is invisible to it; this is the
# one image whose entire purpose is the AWS CLI. Value is the OS bundle that
# update-ca-certificates just refreshed to INCLUDE the staged corp CA, not
# corp-ca.pem alone: AWS_CA_BUNDLE REPLACES the trust store rather than adding
# to it, so the bare corp CA here would make the CLI distrust every public root.
# A Dockerfile ENV cannot sit inside the RUN conditional above, and setting it
# unconditionally is correct anyway — on a non-corp build the OS bundle is
# simply the public roots, so the CLI behaves exactly as it did before.
# (Runtime is separate and additive: dispatch sets AWS_CA_BUNDLE per run when
# TLS is intercepted — installSandboxTrustedCA, internal/api/runs_dispatch_llm.go.)
ENV AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=

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
ARG AWS_CLI_VERSION=2.31.13
ARG TARGETARCH
# Host-staged installer zip (opt-in, gitignored). The README.md decoy makes
# the COPY succeed when the awscliv2-bin* glob matches nothing (the download
# default path) — same trick as claude-code's claude-bin* / codex-bin* globs.
COPY deploy/images/README.md deploy/images/aws-sso/awscliv2-bin* /awscli-stage/
RUN set -eu; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
        amd64) plat=x86_64;; \
        arm64) plat=aarch64;; \
        *) echo "unsupported arch '${TARGETARCH:-unknown}' for AWS CLI" >&2; exit 1;; \
    esac; \
    if [ "$AWS_CLI_INSTALL" = "staged" ]; then \
        [ -f /awscli-stage/awscliv2-bin ] || { echo "AWS_CLI_INSTALL=staged but deploy/images/aws-sso/awscliv2-bin is not staged" >&2; exit 1; }; \
        cp /awscli-stage/awscliv2-bin /tmp/awscliv2.zip; \
        echo "using host-staged AWS CLI installer zip (unverified — trust the staging host)"; \
    else \
        base="https://awscli.amazonaws.com"; \
        url="${base}/awscli-exe-linux-${plat}-${AWS_CLI_VERSION}.zip"; \
        curl -fsSL "$url" -o /tmp/awscliv2.zip; \
        curl -fsSL "${url}.sig" -o /tmp/awscliv2.sig; \
        gpg --batch --no-tty --import /aws-cli-pubkey.asc; \
        gpg --batch --no-tty --verify /tmp/awscliv2.sig /tmp/awscliv2.zip; \
        echo "downloaded + GPG-verified AWS CLI ${AWS_CLI_VERSION} (${plat})"; \
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
# selftest have no terminal to page through); an interactive `wardyn attach`
# shell can still `less` things manually.
ENV AWS_PAGER=""

# Create user `agent` (uid 1000, home /home/agent, work /home/agent/work) —
# debian's useradd, unlike the node:24-bookworm-slim base the sibling images
# use, has no pre-existing uid-1000 user to rename.
# /home/agent/drive is the RESERVED user-drive mount point (runner.DriveTarget),
# created here owned by agent for the copy-up reason deploy/images/base/Dockerfile
# states in full: a fresh managed volume inherits the uid/gid of the image directory
# it is mounted over, so without it a docker_volume drive's root is root-owned and a
# writable managed drive is read-only in practice for uid 1000.
RUN groupadd -g 1000 agent \
 && useradd -m -u 1000 -g agent -d /home/agent -s /bin/bash agent \
 && mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent

# Copy the statically-linked binaries from the build stage.
COPY --from=builder /out/wardyn-rec        /usr/local/bin/wardyn-rec
COPY --from=builder /out/wardyn-git-helper /usr/local/bin/wardyn-git-helper
# wardyn-aws-sso: reads the ~/.aws/sso/cache file `aws sso login` just wrote and
# uploads it through the proxy's brokered sso-token route. The login is only half
# the flow — without this binary the captured token dies with the throwaway
# container, and the UI login pane (which types `aws sso login … && wardyn-aws-sso`
# into the attach PTY) waits forever for its success marker.
COPY --from=builder /out/wardyn-aws-sso    /usr/local/bin/wardyn-aws-sso

# agent-run: the thin shell wrapper the Wardyn driver uses to launch/attach.
# agent-run-lib.sh: functions shared with every other agent image's agent-run.
COPY deploy/images/common/agent-run-lib.sh /usr/local/bin/agent-run-lib.sh
COPY deploy/images/aws-sso/agent-run /usr/local/bin/agent-run
# The chained login command, defined once: agent-run sources it for the variable
# and the interactive attach shell sources it for the hint it prints (the common
# attach-bashrc.sh picks up this path when an image ships it).
COPY deploy/images/aws-sso/login-hint.sh /usr/local/lib/wardyn-attach-hint.sh
# The sign-in pane's body — what `agent-run --idle` runs in the `wardyn` tmux
# session, and therefore what every attach path joins. A FILE, not an inline
# `sh -c '…'`: it quotes operator-facing prose, and one apostrophe would split it.
COPY deploy/images/aws-sso/signin-pane.sh /usr/local/bin/signin-pane.sh
# ONE script, TWO names. This image has no coding agent; `command not found` is
# the shell's answer to a typo, and the operator who typed `claude` here read it
# (reasonably) as a misconfigured run. These answer the question instead, and
# fail closed so nothing mistakes them for a working agent.
COPY deploy/images/aws-sso/not-a-coding-agent.sh /usr/local/bin/claude
COPY deploy/images/aws-sso/not-a-coding-agent.sh /usr/local/bin/codex
# tmux config for the persistent attach session: no status bar (clean terminal,
# no xterm.js line-drawing artifacts), truecolor, large scrollback.
COPY deploy/images/common/tmux.conf /etc/tmux.conf
# Interactive attach shell sources this: waits for workspace prep to finish
# (mirrors claude-code/codex-cli) so `wardyn attach` never lands mid-setup.
COPY --chown=agent:agent deploy/images/common/attach-bashrc.sh /home/agent/.bashrc
RUN chmod +x /usr/local/bin/wardyn-rec \
           /usr/local/bin/wardyn-git-helper \
           /usr/local/bin/wardyn-aws-sso \
           /usr/local/bin/signin-pane.sh \
           /usr/local/bin/claude \
           /usr/local/bin/codex \
           /usr/local/bin/agent-run

# System-wide GLOBAL git credential helper (all hosts) — same wiring as the
# sibling images; this image's own job never clones a repo, but the contract
# is uniform across agent images. See deploy/images/README.md for the flow.
RUN git config --system credential.helper \
        '/usr/local/bin/wardyn-git-helper --secret-file /home/agent/.wardyn/git-helper.secret'

WORKDIR /home/agent/work

USER agent

# No ENTRYPOINT on purpose (see contract above). Both drivers replace this CMD
# at container create (`idleCmd` in CreateSandbox, docker and k8s), so an
# interactive Wardyn run boots `agent-run --idle` — which starts the sign-in
# itself — and never reaches the line below. This CMD is the bare-`docker run`
# fallback only, and it says so rather than telling a Wardyn operator to type a
# command the sandbox now runs for them.
ENTRYPOINT []
CMD ["/bin/bash", "-c", "echo 'wardyn aws-sso agent: no Wardyn run launched this container; `agent-run --idle` is what starts the sign-in' && sleep infinity"]
