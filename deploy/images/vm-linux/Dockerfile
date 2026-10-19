# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The reference guest image of a Linux VM sandbox (machine_kind=linux_vm): an ordinary OCI image
# that wardyn-runnerd converts to an ext4 disk and boots under QEMU/KVM. No desktop. The image
# contract it meets (docs/LINUX-VM-SANDBOXES.md): one /boot/vmlinuz-* and /boot/initrd.img-*, root
# on /dev/vda, systemd, qemu-guest-agent, the Wardyn guest agent with its unit, the CA install unit,
# and the agent user (uid 1000, home /home/agent).
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder
WORKDIR /build
ENV GOTOOLCHAIN=local
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/wardyn-guest-agent ./cmd/wardyn-guest-agent

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
# No console login (the serial console is the runner's log) and no network configuration here.
RUN set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; \
    apt-get install -y --no-install-recommends linux-image-amd64 initramfs-tools systemd systemd-sysv udev \
      qemu-guest-agent util-linux-extra ca-certificates bash procps; \
    apt-get clean; rm -rf /var/lib/apt/lists/*; \
    printf 'qemu_fw_cfg\n' > /etc/modules-load.d/wardyn.conf; \
    systemctl mask getty@.service serial-getty@.service systemd-networkd-wait-online.service
COPY --from=builder /out/wardyn-guest-agent /usr/local/bin/wardyn-guest-agent
COPY deploy/images/vm-linux/wardyn-guest-agent.service deploy/images/vm-linux/wardyn-ca.service /etc/systemd/system/
COPY deploy/images/vm-linux/wardyn-ca /usr/local/sbin/wardyn-ca
COPY deploy/images/vm-linux/10-wardyn-link.network /etc/systemd/network/10-wardyn-link.network
# The agent user, as an agent image has it; the drive root exists for a later managed drive.
RUN useradd -m -u 1000 -U -s /bin/bash agent \
 && mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent
RUN systemctl enable wardyn-guest-agent.service wardyn-ca.service systemd-networkd.service
