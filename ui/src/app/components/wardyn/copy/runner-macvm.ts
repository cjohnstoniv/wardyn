/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// macOS virtual machines on a Mac runner (mocks M-R1 runner drawer, M-R2 runner card badge, approved
// 2026-10-25). Byte for byte from the boards; the id in each comment is the board's string id. A backtick
// pair marks inline code, which MacVMText renders. Lazy-only, reached through the runners screens.
// Not shipped: MV-R42 (the newer-macOS notice has no producer this release).

export const MACVM_R = {
  /** MV-R00, section label */
  R00: "macOS virtual machines",
  /** MV-R01, phase */
  R01: "Checking this Mac",
  /** MV-R02, phase */
  R02: (version: string, size: string) => `Downloading macOS ${version} from Apple (${size})`,
  /** MV-R03, phase */
  R03: "Installing macOS",
  /** MV-R04, phase */
  R04: "Setting up the base image",
  /** MV-R05, phase */
  R05: "Installing developer tools",
  /** MV-R06, phase / ready */
  R06: (guestOs: string) => `Ready: ${guestOs}`,
  /** MV-R10, chip: preparing */
  R10: "Preparing",
  /** MV-R11, chip: waiting */
  R11: "Waiting",
  /** MV-R12, chip: paused */
  R12: "Paused",
  /** MV-R13, chip: needs someone at the Mac (conditional copy: ships only under its owner option) */
  R13: "Needs you at the Mac",
  /** MV-R14, chip: can't offer */
  R14: "Not offered",
  /** MV-R15, chip: failed */
  R15: "Failed",
  /** MV-R16, chip: update */
  R16: "Update available",
  /** MV-R17, chip: update needed */
  R17: "Update needed",
  /** MV-R18, chip: needs a restart */
  R18: "Needs a restart",
  /** MV-R19, chip: desktop view only (conditional copy: ships only under its owner option) */
  R19: "Desktop: view only",
  /** MV-R20, ready: fact label */
  R20: "Guest",
  /** MV-R21, ready: fact label */
  R21: "Smallest run it can take",
  /** MV-R22, ready: fact label */
  R22: "Harnesses",
  /** MV-R23, ready: fact label */
  R23: "Desktop",
  /** MV-R24, ready: desktop value */
  R24: "Opens from the run page",
  /** MV-R25, ready: hint */
  R25: "Apple's licence allows 2 macOS VMs on a Mac. Preparing or updating macOS uses one of them.",
  /** MV-R30, off */
  R30: "Off on this Mac. Turn it on with `wardyn-runnerd setup` on the Mac, or with the `MacOSVM` key in its managed preferences.",
  /** MV-R31, waiting for AC power */
  R31: "Preparing macOS waits until this Mac is on power.",
  /** MV-R32, waiting for Docker */
  R32: "Preparing macOS waits for Docker to start on this Mac.",
  /** MV-R33, preparing paused (battery) */
  R33: "Preparing macOS paused because this Mac went on battery. It continues on power.",
  /** MV-R34, downloading */
  R34: (p: string, rate: string, t: string) => `Downloading macOS from Apple: ${p}% (${rate}), about ${t} left.`,
  /** MV-R35, downloading: slot held */
  R35: "This Mac is preparing macOS, which uses one of its 2 VM slots.",
  /** MV-R36, download interrupted */
  R36: (p: string) => `The macOS download stopped at ${p}%. It resumes from there automatically.`,
  /** MV-R37, installing */
  R37: (p: string) => `Installing macOS: ${p}%.`,
  /** MV-R38, macOS asked for setup by hand (only if D1(c)) (conditional copy: ships only under its owner option) */
  R38: "macOS asked for setup by hand. Open the macOS window on this Mac and finish setup once.",
  /** MV-R39, macOS asked for setup by hand (only if D1(c)): hint (conditional copy: ships only under its owner option) */
  R39: "The account name and its one-time password show on the Mac, never here.",
  /** MV-R40, desktop observe-only (D2) (conditional copy: ships only under its owner option) */
  R40: "The macOS desktop isn't available on this Mac yet: macOS turned Screen Sharing on for viewing only.",
  /** MV-R41, desktop observe-only: fix (only if D2(a)) (conditional copy: ships only under its owner option) */
  R41: "Open the macOS window on this Mac and turn on Screen Sharing control once. Until then, macOS VMs run without a desktop.",
  /** MV-R43, update needed */
  R43: "This Mac's runner was upgraded, so the macOS base's tools are being updated. It offers no macOS VMs until that finishes.",
  /** MV-R43, update needed: the base's format is older than the runner's (failure "format"), so only a new prepare makes it usable */
  R43_FORMAT: "This Mac's runner was upgraded and can't use the macOS base it has. Run `wardyn-runnerd macos-vm prepare` to prepare the base again. It offers no macOS VMs until then.",
  /** MV-R44, failed: install */
  R44: (error: string) => `Installing macOS failed: ${error}. The download is kept. Run \`wardyn-runnerd macos-vm prepare\` to install again.`,
  /** MV-R45, failed: download 3 times */
  R45: (error: string) => `The macOS download failed 3 times: ${error}. Run \`wardyn-runnerd macos-vm prepare\` to try again.`,
  /** MV-R46, failed: developer tools */
  R46: (error: string) => `Installing Apple's Command Line Tools in the base image failed: ${error}. macOS VMs on this Mac offer only Your own tools until it succeeds.`,
  /** MV-R47, per-run memory below the minimum */
  R47: (n: string, m: string, runner: string) => `Per-run memory on this runner is ${n} GB; a macOS VM needs at least ${m} GB. Its owner raises it to ${m} GB or more in Settings → My runner, or with \`wardyn runner set ${runner} --per-run-memory ${m}\`.`,
  /** MV-R48, per-run CPU below the minimum */
  R48: (n: string, m: string, runner: string) => `Per-run CPU on this runner is ${n} CPUs; a macOS VM needs at least ${m}. Its owner raises it to ${m} or more in Settings → My runner, or with \`wardyn runner set ${runner} --per-run-cpu ${m}\`.`,
  /** Not on a board: the approved ledger refusal (cmd/wardyn-runnerd macOSVMLedgerDamaged), worded here from reason ledger_damaged */
  LEDGER_DAMAGED: "This Mac's macOS VM ledger is missing or damaged. Reinstall the Wardyn runner package.",
  /** MV-R49, needs a logged-in session */
  R49: "macOS virtual machines run only while someone is logged in on this Mac.",
  /** MV-R50, needs a restart (slot held) */
  R50: "macOS is still holding a VM slot from an earlier run. Restart this Mac to free it.",
  /** MV-R51, too hot */
  R51: "This Mac is too hot to start another macOS VM. Try again when it cools down.",
  /** MV-R52, host macOS too old */
  R52: (v: string) => `macOS virtual machines need macOS 26 or later on this Mac. It runs macOS ${v}.`,
  /** MV-R53, host can't run VMs */
  R53: (reason: string) => `This Mac can't run macOS virtual machines: ${reason}.`,
  /** MV-R54, disk short (prepare) */
  R54: (n: string) => `Not enough free disk: preparing macOS needs about 85 GB and this Mac has ${n} GB free.`,
  /** MV-R55, low memory */
  R55: (n: string, m: string) => `Not enough free memory to start a macOS VM (${n} GB free, ${m} GB needed).`,
} as const;

/** The runner card badge (M-R2). */
export const MACVM_K = {
  /** MV-K01, badge: count */
  K01: (active: string, max: string) => `macOS VMs ${active}/${max}`,
  /** MV-K02, badge: preparing */
  K02: (p: string) => `macOS VMs · preparing ${p}%`,
  /** MV-K03, badge: not offered */
  K03: "macOS VMs · not offered",
} as const;

/** Guest posture and base age on the runner drawer (board J7, rc.9). The id in each comment is the copy sheet's. */
export const MACVM_P = {
  /** MAC-P01, sub-label */
  P01: "Guest posture",
  /** MAC-P02, fact label */
  P02: "System Integrity Protection",
  /** MAC-P03, fact label */
  P03: "Gatekeeper",
  /** MAC-P04, fact label */
  P04: "Configuration profiles",
  /** MAC-P05, fact label */
  P05: "MDM enrolment",
  /** MAC-P06, fact label */
  P06: "Organisation CA",
  /** MAC-P07, values: each is its own string */
  P07_ON: "On",
  P07_OFF: "Off",
  P07_INSTALLED: (n: number) => `${n} installed`,
  P07_NONE: "None",
  P07_NOT_ENROLLED: "Not enrolled",
  P07_ENROLLED: "Enrolled, configuration only",
  P07_TRUSTED: "Trusted",
  P07_NOT_TRUSTED: "Not trusted",
  /** MAC-P08, hint */
  P08: "Read from the base when it was last prepared or edited. To sign-ins that require a compliant device, the guest is an unmanaged device.",
  /** MAC-P09, fact label */
  P09: "Base age",
  /** MAC-P10, value; one day reads "1 day ago" (MAC-P19's ruling) */
  P10: (date: string, days: number) => `Prepared ${date} (${days === 1 ? "1 day" : `${days} days`} ago)`,
  /** MAC-P11, beside the base age */
  P11: (host: string, guest: string) => `This Mac runs macOS ${host}; the guest runs ${guest}.`,
  /** MAC-P12, chip */
  P12: "Base too old",
  /** MAC-P13, the base is older than the organisation allows */
  P13: (days: number, max: number) =>
    `This base was prepared ${days === 1 ? "1 day" : `${days} days`} ago, and your organisation allows ${max}. Its owner runs \`wardyn-runnerd macos-vm prepare --new-image\` on the Mac.`,
  /** MAC-E16, fact label */
  E16: "Last edited",
  /** MAC-E17, value */
  E17: (date: string) => `${date}, on this Mac`,
} as const;
