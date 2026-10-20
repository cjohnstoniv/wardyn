# wardyn/agent-novnc: agent-base plus a minimal X stack, so a run's policy can
# declare a "novnc" UI app (RunPolicySpec.ui_apps) and relay a real graphical
# desktop over the governed UI gateway (internal/api/uigateway.go) — no new
# substrate code and no new network path out of the sandbox, exactly like the
# code-server image beside it.
#
# BASE: `ARG BASE_IMAGE=wardyn/agent-base:local`, NOT agent-claude-code (which
# deploy/images/vscode also defaults away from, for the same reason). Deliberate,
# and the one design decision in this image:
#   * agent-base is the image contract with no vendor CLI, and it is what this
#     project actually PUBLISHES — agent-claude-code is a local build recipe
#     whose licence file ships in neither the image nor this repository.
#   * An X stack layered on node + npm + a vendor CLI is a materially different
#     size and CVE decision from one on the bare contract. A graphical desktop
#     already brings Xvfb, a window manager, x11vnc and websockify; adding a
#     language runtime it never uses is surface for nothing.
# Inheriting vscode's base would have been the path of least resistance and the
# wrong answer. A developer checkout that wants the vendor CLI reachable from
# this image overrides the default explicitly (build it first with
# `make agent-images-core`):
#   make agent-image-novnc BASE_IMAGE=wardyn/agent-claude-code:local
#
# IMAGE CONTRACT (deploy/images/README.md): inherited from agent-base. This
# stage adds no ENTRYPOINT and keeps USER agent (uid 1000).
ARG BASE_IMAGE=wardyn/agent-base:local

# The desktop sign-in helper (cmd/wardyn-signin, issue #2972): standard
# library only, so this stage needs go.mod and the one package, nothing else.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS signin-builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
COPY cmd/wardyn-signin/ ./cmd/wardyn-signin/
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /out/wardyn-signin ./cmd/wardyn-signin

FROM ${BASE_IMAGE}

# Published (release.yml's images-ui-sandbox job, #141). agent-base already
# carries these two COPYs, but scripts/check-image-pins.sh's licence-file gate
# reads each PUBLISHED Dockerfile's own text, not what an inherited layer
# happens to contain, so a published stage restates them. The licences label
# extends the inherited base expression with what THIS stage adds: x11vnc and
# procps (GPL-2.0-only), openbox (GPL-2.0-or-later), websockify (LGPL-3.0),
# noVNC (MPL-2.0), Xvfb/xterm (MIT/X11), and for desktop sign-ins Chromium
# (BSD-3-Clause plus its bundled components' own), cage (MIT), wayvnc (ISC) and
# fonts-liberation (OFL-1.1) — per-package copyright files are the source of each.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/
COPY licenses/texts/ /usr/share/doc/wardyn/licenses/
LABEL org.opencontainers.image.title="agent-novnc" \
      org.opencontainers.image.description="Wardyn agent image: agent-base plus an X/noVNC desktop stack" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND GPL-2.0-only AND GPL-2.0-or-later AND LGPL-3.0-or-later AND MPL-2.0 AND MIT AND BSD-3-Clause AND ISC AND OFL-1.1" \
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
# Desktop sign-ins (#2972) add Chromium, cage, wayvnc and wardyn-signin.
# MEASURED 2026-10-20 on one agent-base (1.13 GB on disk): this image without
# them is 1.67 GB, with them 2.59 GB, so +~920 MB (+~268 MB compressed). That
# is past the ceiling below; whether they move to an image of their own is
# open, and the ceiling is not raised here.
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

# DESKTOP SIGN-INS (issue #2972; off unless the operator sets
# WARDYN_DESKTOP_SIGNINS=on). The person signs in to websites in a browser of
# their own: uid 1001 `person`, no home on disk, every file and socket in the
# per-run store /run/wardyn/signin (a 0700 tmpfs the runner mounts for that
# uid, gone with the container). The person's display is Wayland, not X: cage
# (headless wlroots, pixman) + wayvnc + websockify on unix sockets in the
# store, so the agent's X display, /tmp and loopback carry nothing of it. The
# agent (uid 1000) keeps its own Chromium profile; a shared site's cookies
# reach it only through `wardyn-signin import`. This is a uid boundary inside
# one sandbox, not a sandbox boundary: a kernel privilege escalation crosses it.
#
# Chromium is the browser on both sides. Its sandbox cannot run under the
# container's seccomp profile and no-new-privileges, so both run --no-sandbox
# (cmd/wardyn-signin/serve.go#browserFlags).
RUN set -eu; \
	export DEBIAN_FRONTEND=noninteractive; \
	apt-get update; \
	apt-get install -y --no-install-recommends \
		chromium \
		cage \
		wayvnc \
		fonts-liberation; \
	apt-get clean; \
	rm -rf /var/lib/apt/lists/*; \
	useradd --uid 1001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin person; \
	mkdir -p /run/wardyn; \
	chmod 0755 /run/wardyn; \
	echo "installed the sign-in stack (chromium, cage, wayvnc) and the person user"
COPY --from=signin-builder /out/wardyn-signin /usr/local/bin/wardyn-signin
RUN chmod 0755 /usr/local/bin/wardyn-signin

USER agent

# No ENTRYPOINT/CMD override: agent-base's inherited `ENTRYPOINT [] CMD []`
# already satisfies the no-ENTRYPOINT contract.
