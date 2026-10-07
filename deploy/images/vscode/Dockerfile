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
ARG CODE_SERVER_VERSION=4.139.1
ARG TARGETARCH
# shell-quote inside code-server, replaced with the fixed release. code-server
# 4.139.1 and the newest release (4.140.0) both bundle shell-quote 1.10.0 in
# lib/vscode/node_modules (CVE-2026-102422, CRITICAL; fixed in 1.11.0), so a
# code-server bump cannot clear it. The module is plain JavaScript with no
# dependencies and no install script, so the registry tarball unpacked in
# place of the bundled copy is the whole install: no npm at build time. Its
# sha256 is computed from the registry tarball at build-pin time, like the
# sums above (that tarball's sha512 matched the integrity the registry
# publishes). It happens in the same RUN as the code-server install, so no
# image layer carries 1.10.0. The lines refuse any bundled version other than
# 1.10.0: a changed bundle requires re-inspecting both the module and the
# embedded-code tracking below before removing the replacement.
# The bundled @devcontainers/cli also inlines its own copy; #1904 and the
# parent README track it separately. Replacing this module does not fix it.
ARG SHELL_QUOTE_VERSION=1.11.0
ARG SHELL_QUOTE_SHA256=028be526afacb2bf5c6c008a95d5435b5d205135f68bb48afd0bb947610393f1
RUN set -eu; \
	case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
		amd64) plat=amd64; sum=53029be6c5781b7bca49b815fcc9a2a3fc111813ad8c9965b2c0f0d2985a0674;; \
		arm64) plat=arm64; sum=0edb4b60d9c4744b2dd14b0911e3c2e6dd8c6f3c13bd58bda23ae744e59e7df1;; \
		*) echo "unsupported arch '${TARGETARCH:-unknown}' for code-server" >&2; exit 1;; \
	esac; \
	url="https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server-${CODE_SERVER_VERSION}-linux-${plat}.tar.gz"; \
	curl -fsSL "$url" -o /tmp/code-server.tar.gz; \
	echo "$sum  /tmp/code-server.tar.gz" | sha256sum -c -; \
	mkdir -p /usr/local/lib/code-server; \
	tar -xzf /tmp/code-server.tar.gz -C /usr/local/lib/code-server --strip-components=1; \
	rm -f /tmp/code-server.tar.gz; \
	echo "installed code-server ${CODE_SERVER_VERSION} (${plat}), checksum-verified"; \
	sq=/usr/local/lib/code-server/lib/vscode/node_modules/shell-quote; \
	grep -q '"version": "1.10.0"' "$sq/package.json" || { \
		echo "code-server ${CODE_SERVER_VERSION} does not bundle shell-quote 1.10.0 at $sq: review the shell-quote replacement and #1904" >&2; exit 1; }; \
	curl -fsSL "https://registry.npmjs.org/shell-quote/-/shell-quote-${SHELL_QUOTE_VERSION}.tgz" -o /tmp/shell-quote.tgz; \
	echo "${SHELL_QUOTE_SHA256}  /tmp/shell-quote.tgz" | sha256sum -c -; \
	rm -rf "$sq"; \
	mkdir -m 0755 "$sq"; \
	tar -xzf /tmp/shell-quote.tgz -C "$sq" --strip-components=1 --no-same-owner; \
	rm -f /tmp/shell-quote.tgz; \
	grep -q "\"version\": \"${SHELL_QUOTE_VERSION}\"" "$sq/package.json"; \
	echo "replaced shell-quote 1.10.0 with ${SHELL_QUOTE_VERSION} in code-server, checksum-verified"

# The launcher convention (deploy/images/README.md, docs/POLICIES.md
# `ui_apps[].name`): an app declared `"name": "vscode"` is started by exec'ing
# /usr/local/bin/wardyn-ui-vscode, on demand, no arguments, no tty — see the
# script's own header for the exact contract.
COPY deploy/images/vscode/wardyn-ui-vscode /usr/local/bin/wardyn-ui-vscode
RUN chmod +x /usr/local/bin/wardyn-ui-vscode

USER agent

# No ENTRYPOINT/CMD override: inherited `ENTRYPOINT [] CMD []` from the base
# image already satisfies the no-ENTRYPOINT contract.
