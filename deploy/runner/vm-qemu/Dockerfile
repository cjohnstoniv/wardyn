# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The host side of a Linux VM sandbox (machine_kind=linux_vm): QEMU/KVM, qemu-img, virtiofsd and
# mke2fs. wardyn-runnerd starts every container from this image itself, unprivileged:
#   --user <runner uid> --group-add <kvm gid> --device /dev/kvm --cap-drop ALL
#   --security-opt no-new-privileges --network none --read-only
# so the image holds no setuid helper the runner relies on, and no network tool.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; \
    apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils virtiofsd e2fsprogs libarchive13t64 tar; \
    apt-get clean; rm -rf /var/lib/apt/lists/*
COPY deploy/runner/vm-qemu/wardyn-vm-image /usr/local/bin/wardyn-vm-image
USER 65532:65532
