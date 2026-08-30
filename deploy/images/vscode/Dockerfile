# wardyn/agent-vscode: the claude-code agent image plus a pinned,
# sha256-verified code-server, so a run's policy can declare a "vscode" UI app
# (RunPolicySpec.ui_apps) and relay a real browser IDE over the governed UI
# gateway (Workstream D, internal/api/uigateway.go) — no new substrate code,
# no new network path out of the sandbox.
#
# `FROM wardyn/agent-claude-code:local` (built by deploy/images/claude-code/
# Dockerfile, NOT digest-pinned — a locally-built image tag, same convention
# as deploy/images/full/Dockerfile): the agent harness, wardyn-rec,
# wardyn-git-helper, agent-run and the git-credential wiring are all
# inherited unchanged; this stage only adds code-server and its launcher.
#
# IMAGE CONTRACT (all agent images must satisfy these — see
# deploy/images/README.md): inherited from the base image. This stage adds
# nothing that changes it — no ENTRYPOINT, same USER agent (uid 1000).
FROM wardyn/agent-claude-code:local

# Local build recipe only — never in release.yml's publish matrix (a gate in
# scripts/check-image-pins.sh keeps it out). The licences label extends the
# inherited base expression with what THIS stage adds: code-server (MIT).
LABEL org.opencontainers.image.title="agent-vscode" \
      org.opencontainers.image.description="Wardyn agent image: claude-code plus code-server (local build only)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND MIT AND LicenseRef-Anthropic-Terms" \
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
