# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The reference guest image of a Linux VM sandbox (machine_kind=linux_vm): an ordinary OCI image
# that wardyn-runnerd converts to an ext4 disk and boots under QEMU/KVM. No desktop. The image
# contract it meets (docs/operations/linux-vm-sandboxes.md): one /boot/vmlinuz-* and /boot/initrd.img-*, root
# on /dev/vda, systemd, qemu-guest-agent, the Wardyn guest agent with its unit, the CA install unit,
# the agent user (uid 1000, home /home/agent), and /usr/lib/openssh/sftp-server for folder syncs.
# No swap and no zram (#2972): the guest agent refuses to initialise a guest with any swap on, because
# the desktop's sign-in store (keychain and browser profiles) is a tmpfs that must stay in memory.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS builder
WORKDIR /build
ENV GOTOOLCHAIN=local
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN for i in 1 2 3; do go mod download && break; [ "$i" -lt 3 ] || exit 1; sleep $((i*5)); done
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/wardyn-guest-agent ./cmd/wardyn-guest-agent

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
# No console login (the serial console is the runner's log). The NIC is addressed as the network relay
# expects (10-wardyn-link.network, wardyn-resolv.service).
RUN set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; \
    apt-get install -y --no-install-recommends linux-image-amd64 initramfs-tools systemd systemd-sysv udev \
      qemu-guest-agent util-linux-extra ca-certificates bash procps openssh-sftp-server; \
    apt-get clean; rm -rf /var/lib/apt/lists/*; \
    printf 'qemu_fw_cfg\n' > /etc/modules-load.d/wardyn.conf; \
    systemctl mask getty@.service serial-getty@.service systemd-networkd-wait-online.service
COPY --from=builder /out/wardyn-guest-agent /usr/local/bin/wardyn-guest-agent
COPY deploy/images/vm-linux/wardyn-guest-agent.service deploy/images/vm-linux/wardyn-ca.service deploy/images/vm-linux/wardyn-resolv.service /etc/systemd/system/
COPY deploy/images/vm-linux/wardyn-ca /usr/local/sbin/wardyn-ca
COPY deploy/images/vm-linux/10-wardyn-link.network /etc/systemd/network/10-wardyn-link.network
# The agent user, as an agent image has it; the drive root exists for a later managed drive.
RUN useradd -m -u 1000 -U -s /bin/bash agent \
 && mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent
RUN systemctl enable wardyn-guest-agent.service wardyn-ca.service wardyn-resolv.service systemd-networkd.service
