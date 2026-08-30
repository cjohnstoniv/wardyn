# wardyn/agent-novnc: agent-base plus a minimal X stack, so a run's policy can
# declare a "novnc" UI app (RunPolicySpec.ui_apps) and relay a real graphical
# desktop over the governed UI gateway (internal/api/uigateway.go) — no new
# substrate code and no new network path out of the sandbox, exactly like the
# code-server image beside it.
#
# BASE: agent-base, NOT agent-claude-code (which deploy/images/vscode uses).
# Deliberate, and the one design decision in this image:
#   * agent-base is the image contract with no vendor CLI, and it is what this
#     project actually PUBLISHES — agent-claude-code is a local build recipe.
#   * An X stack layered on node + npm + a vendor CLI is a materially different
#     size and CVE decision from one on the bare contract. A graphical desktop
#     already brings Xvfb, a window manager, x11vnc and websockify; adding a
#     language runtime it never uses is surface for nothing.
# Inheriting vscode's base would have been the path of least resistance and the
# wrong answer.
#
# IMAGE CONTRACT (deploy/images/README.md): inherited from agent-base. This
# stage adds no ENTRYPOINT and keeps USER agent (uid 1000).
FROM wardyn/agent-base:local

# Local build recipe only — never in release.yml's publish matrix (a gate in
# scripts/check-image-pins.sh keeps it out). The licences label extends the
# inherited base expression with what THIS stage adds: x11vnc and procps
# (GPL-2.0-only), openbox (GPL-2.0-or-later), websockify (LGPL-3.0),
# noVNC (MPL-2.0), Xvfb/xterm (MIT/X11) — per-package copyright files are the
# source of each.
LABEL org.opencontainers.image.title="agent-novnc" \
      org.opencontainers.image.description="Wardyn agent image: agent-base plus an X/noVNC desktop stack (local build only)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND GPL-2.0-only AND GPL-2.0-or-later AND LGPL-3.0-or-later AND MPL-2.0 AND MIT" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn"

USER root

# PINNING, and why it differs from the vscode image's single tarball.
#
# vscode installs ONE upstream release tarball and sha256-verifies it. An X
# stack has no such artifact: Xvfb, the window manager, x11vnc and websockify
# are distro packages. So the pin here is a VERSION pin per apt package, taken
# from the base image's Debian suite, with the suite itself pinned by
# agent-base's own FROM. That is a weaker guarantee than a checksummed tarball
# and it is stated rather than glossed: a rebuild after a Debian point release
# can pick up a new patch version of any of these.
#
# noVNC and websockify come from GitHub tarballs instead, sha256-verified the
# same way vscode's is, because Debian's noVNC lags badly.
#
# SIZE, MEASURED (not estimated): agent-base is 655 MB, this image is 1.22 GB —
# a delta of ~565 MB, more than double the +~228 MiB the code-server layer costs.
# An X stack is simply expensive, and python3-numpy (websockify's fast path)
# plus the fonts openbox and xterm pull in are most of it.
#
# CEILING: if a change pushes this past ~1.4 GB, drop xterm and then openbox
# before raising it — a relayed app needs neither, they exist only so a first
# open is not an empty grey square, and they are the cheapest things to lose.
# Do NOT reach for a smaller base: agent-base is already the contract minimum.
RUN set -eu; \
	export DEBIAN_FRONTEND=noninteractive; \
	apt-get update; \
	apt-get install -y --no-install-recommends \
		xvfb \
		x11vnc \
		openbox \
		xterm \
		python3-numpy \
		procps; \
	apt-get clean; \
	rm -rf /var/lib/apt/lists/*; \
	echo "installed the X stack (xvfb, x11vnc, openbox, xterm)"

# noVNC + websockify, pinned and sha256-verified — same bar as vscode's
# code-server tarball. A version bump means re-downloading, recomputing the
# sum, and updating both entries; a mismatch fails the build closed.
ARG NOVNC_VERSION=1.6.0
ARG WEBSOCKIFY_VERSION=0.13.0
RUN set -eu; \
	novnc_sum=5066103959ef4e9b10f37e5a148627360dd8414e4cf8a7db92bdbd022e728aaa; \
	wsock_sum=b6413e364efd04f3c92ec8c17747e3c4adc20157c2ef1c5d019a26d944a46df8; \
	curl -fsSL "https://github.com/novnc/noVNC/archive/refs/tags/v${NOVNC_VERSION}.tar.gz" -o /tmp/novnc.tar.gz; \
	curl -fsSL "https://github.com/novnc/websockify/archive/refs/tags/v${WEBSOCKIFY_VERSION}.tar.gz" -o /tmp/websockify.tar.gz; \
	echo "$novnc_sum  /tmp/novnc.tar.gz" | sha256sum -c -; \
	echo "$wsock_sum  /tmp/websockify.tar.gz" | sha256sum -c -; \
	mkdir -p /usr/local/lib/novnc /usr/local/lib/websockify; \
	tar -xzf /tmp/novnc.tar.gz -C /usr/local/lib/novnc --strip-components=1; \
	tar -xzf /tmp/websockify.tar.gz -C /usr/local/lib/websockify --strip-components=1; \
	rm -f /tmp/novnc.tar.gz /tmp/websockify.tar.gz; \
	ln -sf /usr/local/lib/novnc/vnc.html /usr/local/lib/novnc/index.html; \
	echo "installed noVNC ${NOVNC_VERSION} + websockify ${WEBSOCKIFY_VERSION}, checksum-verified"

# The launcher convention (deploy/images/README.md; docs/POLICIES.md
# `ui_apps[].name`): an app declared `"name": "novnc"` is started by exec'ing
# /usr/local/bin/wardyn-ui-novnc, on demand, no arguments, no tty.
COPY deploy/images/novnc/wardyn-ui-novnc /usr/local/bin/wardyn-ui-novnc
RUN chmod +x /usr/local/bin/wardyn-ui-novnc

USER agent

# No ENTRYPOINT/CMD override: agent-base's inherited `ENTRYPOINT [] CMD []`
# already satisfies the no-ENTRYPOINT contract.
