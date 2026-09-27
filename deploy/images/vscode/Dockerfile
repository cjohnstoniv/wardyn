# wardyn/agent-vscode: agent-base plus a pinned, sha256-verified code-server,
# so a run's policy can declare a "vscode" UI app (RunPolicySpec.ui_apps) and
# relay a real browser IDE over the governed UI gateway (Workstream D,
# internal/api/uigateway.go) — no new substrate code, no new network path out
# of the sandbox. The launcher below only execs code-server; it never calls a
# coding-agent CLI.
#
# `ARG BASE_IMAGE=wardyn/agent-base:local` (built by deploy/images/base/
# Dockerfile, NOT digest-pinned — a locally-built image tag): agent-base is
# the image contract with no vendor CLI, and it is what this project actually
# publishes. `FROM wardyn/agent-claude-code:local` would bundle Anthropic's
# proprietary Claude Code CLI, whose licence file ships in neither the image
# nor this repository — LICENSING.md's claim that no published image bundles
# one depends on this default staying agent-base. A developer checkout that
# wants `claude` available in the vscode terminal restores it explicitly:
#   make agent-image-vscode BASE_IMAGE=wardyn/agent-claude-code:local
# (build the vendor base first with `make agent-images-core`).
#
# IMAGE CONTRACT (all agent images must satisfy these — see
# deploy/images/README.md): inherited from the base image. This stage adds
# nothing that changes it — no ENTRYPOINT, same USER agent (uid 1000).
ARG BASE_IMAGE=wardyn/agent-base:local
FROM ${BASE_IMAGE}

# Published (release.yml's images-ui-sandbox job, #141). agent-base already
# carries these two COPYs, but scripts/check-image-pins.sh's licence-file gate
# reads each PUBLISHED Dockerfile's own text, not what an inherited layer
# happens to contain, so a published stage restates them. The licences label
# extends the inherited base expression with what THIS stage adds: code-server
# (MIT).
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/
COPY licenses/texts/ /usr/share/doc/wardyn/licenses/
LABEL org.opencontainers.image.title="agent-vscode" \
      org.opencontainers.image.description="Wardyn agent image: agent-base plus code-server" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND MIT" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn"

USER root

# code-server release tarball, sha256-verified against a checksum computed
# from the official GitHub release asset at build-pin time (coder/code-server
# publishes no separate checksums file for this asset, unlike claude-code's
# manifest.json or aws-sso's GPG signature — pinning our own known-good sum is
# the equivalent bar: a version bump means re-downloading the new tarball,
# recomputing its sha256, and updating both entries below; a mismatch fails
# the build closed, never silently).
ARG CODE_SERVER_VERSION=4.133.0
ARG TARGETARCH
RUN set -eu; \
	case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
		amd64) plat=amd64; sum=a4e0f8f8c76e7de8e7424289f74e507af4c97bfe104c3e8ee272b8cc7b46c6f1;; \
		arm64) plat=arm64; sum=d999d8b0256e5537f3b62e6c09f624220026e19107a04a876c0cef62d1c71147;; \
		*) echo "unsupported arch '${TARGETARCH:-unknown}' for code-server" >&2; exit 1;; \
	esac; \
	url="https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-${plat}.tar.gz"; \
	curl -fsSL "$url" -o /tmp/code-server.tar.gz; \
	echo "$sum  /tmp/code-server.tar.gz" | sha256sum -c -; \
	mkdir -p /usr/local/lib/code-server; \
	tar -xzf /tmp/code-server.tar.gz -C /usr/local/lib/code-server --strip-components=1; \
	rm -f /tmp/code-server.tar.gz; \
	echo "installed code-server ${CODE_SERVER_VERSION} (${plat}), checksum-verified"

# The launcher convention (deploy/images/README.md, docs/POLICIES.md
# `ui_apps[].name`): an app declared `"name": "vscode"` is started by exec'ing
# /usr/local/bin/wardyn-ui-vscode, on demand, no arguments, no tty — see the
# script's own header for the exact contract.
COPY deploy/images/vscode/wardyn-ui-vscode /usr/local/bin/wardyn-ui-vscode
RUN chmod +x /usr/local/bin/wardyn-ui-vscode

USER agent

# No ENTRYPOINT/CMD override: inherited `ENTRYPOINT [] CMD []` from the base
# image already satisfies the no-ENTRYPOINT contract.
