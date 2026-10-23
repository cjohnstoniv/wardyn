/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step's machine kind (mock M5, option A as revised: M5-Revised). Byte for byte from the board.
// Lazy-only, like the other New Run copy modules.

export const NR_MACHINE = {
  LABEL: "Machine kind",
  CONTAINER: "Container",
  VM: "Virtual machine",
  CONTAINER_HELP: "A sandbox container with a terminal. This is the default.",
  VM_HELP: "A full desktop in its own virtual machine, with a browser. You open it from the run page.",
  OS_LABEL: "Operating system",
  OS_LINUX: "Linux",
  OS_LATER: "macOS and Windows virtual machines arrive when your organisation adds a pool that can run them.",
  BARRIER_LABEL: "Barrier",
  BARRIER_VM: "The virtual machine",
  BARRIER_VM_WHY: "Its own machine with its own kernel, so there is nothing to pick. Fence, Wall and Vault are container barriers.",
  CHANGES_TITLE: "What changes with a desktop VM",
  CHANGES_NETWORK: "Network",
  CHANGES_NETWORK_BODY: "Same as a container. Every connection goes through the run's proxy.",
  CHANGES_CLIPBOARD: "Clipboard",
  // Not the board's "Paste in is allowed. Copy out is blocked.": QEMU's VNC has no clipboard agent in the
  // guest in this release, so nothing is copied either way (PR "Copy not on the boards").
  CHANGES_CLIPBOARD_BODY:
    "Copy and paste aren't available on VM desktops yet. When they are, the run's paste in and copy out rules apply. The filter is not a boundary against a run built to get data out: the run controls what its screen shows.",
} as const;
