# wardyn/agent-harnesses: one sandbox image carrying Claude Code AND Codex.
#
# LOCAL BUILD ONLY (`make agent-image-harnesses`). It layers on the claude-code
# image, which conveys Anthropic's proprietary CLI, so it is never published: it
# is in no release or nightly matrix, and scripts/test-image-recipes.py fails if
# it appears in one. See LICENSING.md.
#
# An operator declares it with a "+" key in WARDYN_AGENT_IMAGES, e.g.
#   "claude-code+codex-cli": "wardyn/agent-harnesses:local"
# and dispatch tells it which harness to start through WARDYN_HARNESSES and
# WARDYN_START_HARNESS. /usr/local/bin/agent-run is the set launcher
# (deploy/images/common/agent-run-set); each harness's own launcher sits under
# /usr/local/lib/wardyn/harness/<id>/agent-run.
#
# Layer on another base (it must already carry `claude`, node/npm and the Wardyn
# image contract) with:  --build-arg BASE_IMAGE=wardyn/agent-full:local
#
# Not digest-pinned: a locally-built image tag, not a registry image.
ARG BASE_IMAGE=wardyn/agent-claude-code:local
FROM ${BASE_IMAGE}

LABEL org.opencontainers.image.title="agent-harnesses" \
      org.opencontainers.image.description="Wardyn agent image: claude-code plus codex-cli in one sandbox (local build only)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND LicenseRef-Anthropic-Terms" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn" \
      io.wardyn.harnesses="claude-code codex-cli"

USER root

# Same build-only ARGs and the same Codex pin as deploy/images/codex-cli/Dockerfile
# (scripts/test-image-recipes.py fails if the two defaults differ). Bump them
# together with the THIRD-PARTY-TERMS.md row.
ARG NPM_REGISTRY=
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG NO_PROXY=
ARG CODEX_INSTALL=npm
ARG CODEX_VERSION=0.149.1
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

# The shared library, both harness launchers and the set launcher come from THIS
# checkout, so they match each other whatever the base image was built from.
COPY deploy/images/common/agent-run-lib.sh /usr/local/bin/agent-run-lib.sh
COPY deploy/images/claude-code/agent-run /usr/local/lib/wardyn/harness/claude-code/agent-run
COPY deploy/images/codex-cli/agent-run /usr/local/lib/wardyn/harness/codex-cli/agent-run
COPY deploy/images/common/agent-run-set /usr/local/bin/agent-run
COPY --chown=agent:agent deploy/images/common/attach-bashrc.sh /home/agent/.bashrc
RUN chmod +x /usr/local/bin/agent-run \
             /usr/local/lib/wardyn/harness/claude-code/agent-run \
             /usr/local/lib/wardyn/harness/codex-cli/agent-run \
 && command -v claude && command -v codex && codex --version

USER agent
