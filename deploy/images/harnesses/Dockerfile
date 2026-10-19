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
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND MIT AND LicenseRef-Anthropic-Terms" \
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
# A staged binary built for the other arch (see stage-agent-binary.sh --arch) is refused by its ELF
# machine type (byte 18: 0x3e amd64, 0xb7 arm64), as claude-code's is. Anything else (not ELF, such as
# a launcher script, or another machine type) installs as before.
ARG TARGETARCH
COPY deploy/images/README.md deploy/images/codex-cli/codex-bin* /codex-stage/
RUN set -eu; \
    if [ "$CODEX_INSTALL" = "native" ]; then \
        [ -f /codex-stage/codex-bin ] || { echo "CODEX_INSTALL=native but deploy/images/codex-cli/codex-bin is not staged — stage it first (see deploy/images/README.md)" >&2; exit 1; }; \
        want="${TARGETARCH:-$(dpkg --print-architecture)}"; \
        case "$(od -An -tx1 -j18 -N1 /codex-stage/codex-bin | tr -d ' ')" in 3e) have=amd64;; b7) have=arm64;; *) have=unknown;; esac; \
        [ "$have" = unknown ] || [ "$have" = "$want" ] || { echo "staged codex-bin is $have but this image builds for $want; restage with: scripts/stage-agent-binary.sh codex-cli --arch $want" >&2; exit 1; }; \
        install -m 0755 /codex-stage/codex-bin /usr/local/bin/codex; \
        echo "installed host-staged native codex"; \
    else \
        if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi; \
        if [ -n "$HTTP_PROXY" ]; then npm config set proxy "$HTTP_PROXY"; fi; \
        if [ -n "$HTTPS_PROXY" ]; then npm config set https-proxy "$HTTPS_PROXY"; fi; \
        npm install -g "@openai/codex@$CODEX_VERSION"; \
        npm config delete proxy https-proxy; \
        echo "installed codex $CODEX_VERSION via npm"; \
    fi

# ── Azure CLI (az) (the Azure CLI component, cli.kind azure) ─────────────────
# From Microsoft's apt repository. The repository's signing key is checked by
# FINGERPRINT (Microsoft (Release signing), key id EB3E94ADBE1229CF) rather than
# by a digest of the key file, which changes whenever Microsoft re-exports the
# key; a key that is not that one, or a file carrying a second key, fails the
# build. signed-by makes the key vouch for this repository alone, and the
# version is pinned: a bump moves AZURE_CLI_VERSION here and in every other
# image that carries this block (scripts/test-image-recipes.py fails when the
# copies drift). The suite is bookworm on this trixie base: Microsoft publishes
# azure-cli for bookworm and for no later Debian release, and the package
# carries its own Python, so it installs and runs here unchanged. gnupg and
# wget are removed again unless the image already had them. az gets no Azure
# credential here or at run time: a run with an Azure CLI component gets a
# profile and an inert token cache, and wardyn-proxy adds the token to each
# request it lets through.
ARG AZURE_CLI_VERSION=2.91.0
RUN set -eu; \
    had_gpg=1; command -v gpg >/dev/null || had_gpg=0; \
    had_wget=1; command -v wget >/dev/null || had_wget=0; \
    apt-get update; apt-get install -y --no-install-recommends ca-certificates gnupg wget; \
    install -d /etc/apt/keyrings; export GNUPGHOME=/tmp/az-gnupg; install -d -m 0700 "$GNUPGHOME"; \
    wget -qO /tmp/microsoft.asc https://packages.microsoft.com/keys/microsoft.asc; \
    keys="$(gpg --show-keys --with-colons /tmp/microsoft.asc)"; \
    [ "$(printf '%s\n' "$keys" | grep -c '^pub:')" = 1 ] || { echo "Microsoft's package signing key file does not hold exactly one key" >&2; exit 1; }; \
    fpr="$(printf '%s\n' "$keys" | awk -F: '$1 == "fpr" { print $10; exit }')"; \
    [ "$fpr" = "BC528686B50D79E339D3721CEB3E94ADBE1229CF" ] || { echo "Microsoft's package signing key is not the pinned one: $fpr" >&2; exit 1; }; \
    gpg --dearmor -o /etc/apt/keyrings/microsoft.gpg /tmp/microsoft.asc; \
    rm -rf /tmp/microsoft.asc "$GNUPGHOME"; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/microsoft.gpg] https://packages.microsoft.com/repos/azure-cli/ bookworm main" > /etc/apt/sources.list.d/azure-cli.list; \
    # Microsoft's repository is trusted for azure-cli only, never for any other package name.
    printf 'Package: *\nPin: origin packages.microsoft.com\nPin-Priority: -1\n\nPackage: azure-cli\nPin: origin packages.microsoft.com\nPin-Priority: 500\n' > /etc/apt/preferences.d/azure-cli; \
    apt-get update; apt-get install -y --no-install-recommends "azure-cli=${AZURE_CLI_VERSION}-1~bookworm"; \
    [ "$had_gpg" = 1 ] || apt-get purge -y --auto-remove gnupg; \
    [ "$had_wget" = 1 ] || apt-get purge -y --auto-remove wget; \
    rm -rf /var/lib/apt/lists/*; \
    command -v az

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
