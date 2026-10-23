# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Real editor clients for scripts/run-e2e-ssh.sh's editors mode (make
# test-e2e-ssh-editors): they run here, never on the host. Each client is
# pinned by exact version and sha256; a bump changes both lines together.
# The base is the digest the agent images already use, so it is no new pull.
FROM node:24-trixie-slim@sha256:8ec5d7557396cfe32d21c3f9c13072355ceab22b584578ca4bb28af31120cffe

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl openssh-client openssl \
    && rm -rf /var/lib/apt/lists/*

# Mutagen 0.18.1. The archive carries the client and mutagen-agents.tar.gz, which
# the client reads from beside its own binary and pushes to the remote.
ADD --checksum=sha256:7735286c778cc438418209f24d03a64f3a0151c8065ef0fe079cfaf093af6f8f \
    https://github.com/mutagen-io/mutagen/releases/download/v0.18.1/mutagen_linux_amd64_v0.18.1.tar.gz /tmp/mutagen.tar.gz
RUN mkdir -p /opt/mutagen && tar -xzf /tmp/mutagen.tar.gz -C /opt/mutagen && rm /tmp/mutagen.tar.gz

COPY run.sh /usr/local/bin/editors-run
ENTRYPOINT ["editors-run"]
