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
  // macOS virtual machines (MV-N*, MV-C*, MV-E*): the strings are the approved canon, ids in the comments.
  OS_MACOS: "macOS", // MV-N01
  OS_WINDOWS_LATER: "Windows virtual machines arrive when your organisation adds a pool that can run them.", // MV-N02
  MACOS_ABOUT: (guestOS: string) => `${guestOS}, set up on the Mac itself from Apple's download. Each Mac runs at most 2.`, // MV-N03
  MACOS_SIZE: (cpu: string, mem: string) => `CPU and memory start at the macOS minimum, ${cpu} CPU and ${mem} MiB. You can raise them on Review.`, // MV-N04
  MACOS_POOL_NOT_OFFERED: "Every runner in this pool must offer macOS VMs, and at least one doesn't. Choose a pool of Macs that offer them, or ask an admin to make one.", // MV-N05
  MACOS_POOL_CANT_NOW: "Every runner in this pool must be able to take a macOS VM, and at least one can't right now. Try again later, or choose another pool.", // MV-N14
  MACOS_PREPARING: (runner: string, percent: number) => `${runner} is still preparing macOS (${percent}%).`, // MV-N06
  MACOS_AT_LIMIT: (runner: string) => `${runner} already runs 2 macOS VMs, the most Apple's licence allows.`, // MV-N07
  MACOS_NOT_MANAGED_OPERATOR: (runner: string, id: string) => `${runner} is not marked as company-managed. Mark it with \`wardyn runner trust ${id} managed_device\`.`, // MV-N08
  MACOS_NOT_MANAGED_MEMBER: (runner: string) => `${runner} is not marked as company-managed. Ask a security operator to mark it.`, // MV-N09
  MACOS_NO_POSTURE: (runner: string, path: string) => `${runner} has no MDM posture file saying it is managed (\`${path}\`).`, // MV-N10
  MACOS_HARNESS_ONE: (runner: string) => `macOS VMs on ${runner} offer only Your own tools until Apple's Command Line Tools finish installing.`, // MV-N11
  MACOS_HARNESS_POOL: (harness: string) => `Every runner in this pool must offer ${harness} in a macOS VM. Until Apple's Command Line Tools finish installing on each Mac, choose Your own tools.`, // MV-N15
  MACOS_MINIMUM_MARK: "macOS minimum", // MV-N12
  MACOS_BELOW_MINIMUM: (cpu: string, mem: string) => `A macOS VM needs at least ${cpu} CPU and ${mem} MiB.`, // MV-N13
  MACOS_NO_BASE: (runner: string) => `${runner} has no macOS base image ready yet.`, // MV-E03
  MACOS_NO_BASE_ERROR: (runner: string, error: string) => `${runner} has no macOS base image ready yet: ${error}.`, // MV-E04
  MACOS_NO_ROOM: (runner: string) => `${runner} has no room for another macOS VM right now.`, // MV-E05
  MACOS_NOT_OFFERED: (runner: string) => `${runner} doesn't offer macOS VMs: it is older than 0.10, or macOS VMs aren't turned on for it.`, // MV-E07
  MACOS_OFF: "macOS virtual machines are turned off in this organisation.", // MV-E01
  MACOS_NOT_REGISTERED: "macOS VMs run only on a registered runner that offers them.", // MV-E06
  MACOS_STALE_HARNESS: (harness: string) => `This Mac's macOS base can't run ${harness} right now.`, // MV-E02
  CHANGES_SIGN_INS: "Sign-ins", // MV-C03
  CHANGES_SIGN_INS_RUN_DISK: "Kept on this run's virtual disk and removed from this Mac when the run ends. The Mac's own snapshots or backups may keep a copy for a while.", // MV-C04
  CHANGES_SIGN_INS_OFF: (agent: string) => `Your admin doesn't allow sign-ins in a desktop while ${agent} runs, and a macOS desktop can't block them yet. Choose Linux or another agent.`, // MV-C07
  CHANGES_MAC: "Mac", // MV-C05
  CHANGES_MAC_MANAGED: "Runs on a Mac your administrators marked as company-managed.", // MV-C06
  // MV-C01 and MV-C02 are the two clipboard variants; the macOS rental decides which ships. The panel shows
  // MV-C02 until it does, the one that claims no rule acts.
  CHANGES_CLIPBOARD_MACOS_RULES: "The run's paste in and copy out rules apply. The filter is not a boundary against a run built to get data out: the run controls what its screen shows.", // MV-C01
  CHANGES_CLIPBOARD_MACOS_NONE: "Copy and paste aren't available on macOS desktops. Nothing is copied either way.", // MV-C02
} as const;
