# wardyn/agent-novnc-signin: agent-novnc plus desktop sign-ins (issue #2972).
# A separate image so the default desktop stays small: wardynd dispatches a run
# on this image only when WARDYN_DESKTOP_SIGNINS=on and the run's policy
# declares the novnc desktop (internal/api/run_desktop_signin.go,
# desktopSigninsFor). With the knob off nothing pulls it.
#
# BASE: `ARG BASE_IMAGE=wardyn/agent-novnc:local`, built by
# deploy/images/novnc/Dockerfile (`make agent-image-novnc`). Release builds pass
# the agent-novnc ref of the same release.
#
# IMAGE CONTRACT (deploy/images/README.md): inherited from agent-base through
# agent-novnc. This stage adds no ENTRYPOINT and keeps USER agent (uid 1000).
ARG BASE_IMAGE=wardyn/agent-novnc:local

# The desktop sign-in helper (cmd/wardyn-signin): standard library only, so
# this stage needs go.mod and the one package, nothing else.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS signin-builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
COPY cmd/wardyn-signin/ ./cmd/wardyn-signin/
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /out/wardyn-signin ./cmd/wardyn-signin

FROM ${BASE_IMAGE}

# Published (release.yml), so this stage restates the licence COPYs that
# scripts/check-image-pins.sh reads from each published Dockerfile's own text.
# The licences label extends agent-novnc's expression with what THIS stage
# adds: Chromium (BSD-3-Clause plus its bundled components' own), cage (MIT),
# wayvnc (ISC) and fonts-liberation (OFL-1.1); per-package copyright files are
# the source of each.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/
COPY licenses/texts/ /usr/share/doc/wardyn/licenses/
LABEL org.opencontainers.image.title="agent-novnc-signin" \
      org.opencontainers.image.description="Wardyn agent image: agent-novnc plus desktop sign-ins (a browser of the person's own)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later AND GPL-2.0-only AND GPL-2.0-or-later AND LGPL-3.0-or-later AND MPL-2.0 AND MIT AND BSD-3-Clause AND ISC AND OFL-1.1" \
      org.opencontainers.image.source="https://github.com/cjohnstoniv/wardyn"

USER root

# The person signs in to websites in a browser of their own: uid 1001
# `person`, no home on disk, every file and socket in the per-run store
# /run/wardyn/signin (a 0700 tmpfs the runner mounts for that uid, gone with
# the container). The person's display is Wayland, not X: cage (headless
# wlroots, pixman) + wayvnc + websockify on unix sockets in the store, so the
# agent's X display, /tmp and loopback carry nothing of it. The agent (uid
# 1000) keeps its own Chromium profile; a shared site's cookies reach it only
# through `wardyn-signin import`. This is a uid boundary inside one sandbox,
# not a sandbox boundary: a kernel privilege escalation crosses it.
#
# Chromium is the browser on both sides. Its sandbox cannot run under the
# container's seccomp profile and no-new-privileges, so both run --no-sandbox
# (cmd/wardyn-signin/serve.go#browserFlags).
#
# XWAYLAND: the xwayland package is a hard Depends of cage, and Debian 13's
# 2:24.1.6-1 carries CRITICALs with no fixed version (CVE-2026-34000,
# CVE-2026-34002). The package stays installed, so cage's Depends holds, and
# its one binary is deleted: a person's display never runs an X server, and an
# X client that reaches cage's lazy X socket gets a failed spawn, not a server.
# scripts/test-image-novnc-signin.sh asserts the binary is absent;
# .trivyignore.novnc-signin carries the two ids for this image only.
#
# SIZE, MEASURED 2026-10-20 (`docker images`, and `docker save | gzip` for
# compressed) on the wardyn-docker daemon, on an agent-base of 1.13 GB:
# agent-novnc 1.67 GB on disk / 399 MB compressed, this image 2.59 GB /
# 665 MB, so +~920 MB on disk and +~266 MB compressed.
# CEILING: 2.7 GB on disk (the measurement plus ~4%). Past it, drop fonts-liberation first (Chromium
# renders with the base image's fonts), then look at Chromium's locales.
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
	rm -f /usr/bin/Xwayland; \
	test ! -e /usr/bin/Xwayland; \
	useradd --uid 1001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin person; \
	mkdir -p /run/wardyn; \
	chmod 0755 /run/wardyn; \
	echo "installed the sign-in stack (chromium, cage, wayvnc; Xwayland binary removed) and the person user"
COPY --from=signin-builder /out/wardyn-signin /usr/local/bin/wardyn-signin
RUN chmod 0755 /usr/local/bin/wardyn-signin

USER agent

# No ENTRYPOINT/CMD override: agent-base's inherited `ENTRYPOINT [] CMD []`
# already satisfies the no-ENTRYPOINT contract.
