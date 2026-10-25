/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { RUN_STARTUP_MACOS } from "../../../screens/run-status-detail";
import { DESKTOP } from "../desktop";
import { NR_MACHINE } from "./machine";

// The macOS VM strings of the New Run, run page and refusal boards, character for character as the owner approved
// them (the em dashes and full stops included). Editing one is a copy change and needs the mock's owner sign-off.
const CANON: [id: string, actual: string, canon: string][] = [
  ["MV-C01", NR_MACHINE.CHANGES_CLIPBOARD_MACOS_RULES, "The run's paste in and copy out rules apply. The filter is not a boundary against a run built to get data out: the run controls what its screen shows."],
  ["MV-C02", NR_MACHINE.CHANGES_CLIPBOARD_MACOS_NONE, "Copy and paste aren't available on macOS desktops. Nothing is copied either way."],
  ["MV-C03", NR_MACHINE.CHANGES_SIGN_INS, "Sign-ins"],
  ["MV-C04", NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK, "Kept on this run's virtual disk and removed from this Mac when the run ends. The Mac's own snapshots or backups may keep a copy for a while."],
  ["MV-C05", NR_MACHINE.CHANGES_MAC, "Mac"],
  ["MV-C06", NR_MACHINE.CHANGES_MAC_MANAGED, "Runs on a Mac your administrators marked as company-managed."],
  ["MV-C07", NR_MACHINE.CHANGES_SIGN_INS_OFF("Release bot"), "Your admin doesn't allow sign-ins in a desktop while Release bot runs, and a macOS desktop can't block them yet. Choose Linux or another agent."],
  ["MV-D01", DESKTOP.kindMacOS, "macOS desktop VM"],
  ["MV-D02", RUN_STARTUP_MACOS.STEP_CLONE, "Cloning the macOS base"],
  ["MV-D03", RUN_STARTUP_MACOS.STEP_START, "Starting macOS"],
  ["MV-D04", RUN_STARTUP_MACOS.STEP_CONNECT, "Connecting to the VM"],
  ["MV-D05", RUN_STARTUP_MACOS.STEP_CONNECT_FAILED, "Connecting to the VM — failed"],
  ["MV-D06", RUN_STARTUP_MACOS.NO_AGENT, "The macOS VM started but its Wardyn agent did not connect within 3 minutes."],
  ["MV-D07", RUN_STARTUP_MACOS.DISK_SHORT("51", "38"), "Not enough free disk to start a macOS VM safely: it reserves 51 GB and this Mac has 38 GB free."],
  ["MV-D08", DESKTOP.signInsNoteRunDisk, "Sign-ins you make in this desktop are kept on this run's virtual disk and removed from this Mac when the run ends. The Mac's own snapshots or backups may keep a copy for a while. While the run goes on, the agent can use them too. Closing this tab doesn't remove them."],
  ["MV-D09", DESKTOP.observeOnlyMacOS, "The macOS desktop isn't available on this Mac yet: macOS turned Screen Sharing on for viewing only."],
  ["MV-D10", DESKTOP.diskRemoving, "Removing the macOS VM's disk from this Mac…"],
  ["MV-D11", DESKTOP.diskRemoved("14:32"), "The macOS VM and its disk were removed from this Mac at 14:32."],
  ["MV-D12", DESKTOP.diskRemoveFailed("the run dir is still in use"), "The macOS VM's disk could not be removed from this Mac: the run dir is still in use."],
  ["MV-D13", DESKTOP.endedLowDisk("4"), "This Mac ran low on disk (4 GB free), so the macOS VM was stopped."],
  ["MV-D14", DESKTOP.endedRunnerUpgraded, "This Mac's runner was upgraded while the macOS VM ran, so the run was ended."],
  ["MV-D15", DESKTOP.startingMacOS, "The macOS desktop is still starting."],
  ["MV-D16", RUN_STARTUP_MACOS.STEP_START_FAILED, "Starting macOS — failed"],
  ["MV-E01", NR_MACHINE.MACOS_OFF, "macOS virtual machines are turned off in this organisation."],
  ["MV-E02", NR_MACHINE.MACOS_STALE_HARNESS("Claude Code"), "This Mac's macOS base can't run Claude Code right now."],
  ["MV-E03", NR_MACHINE.MACOS_NO_BASE("mac-studio-07"), "mac-studio-07 has no macOS base image ready yet."],
  ["MV-E04", NR_MACHINE.MACOS_NO_BASE_ERROR("mac-studio-07", "disk full"), "mac-studio-07 has no macOS base image ready yet: disk full."],
  ["MV-E05", NR_MACHINE.MACOS_NO_ROOM("mac-studio-07"), "mac-studio-07 has no room for another macOS VM right now."],
  ["MV-E06", NR_MACHINE.MACOS_NOT_REGISTERED, "macOS VMs run only on a registered runner that offers them."],
  ["MV-E07", NR_MACHINE.MACOS_NOT_OFFERED("mac-studio-07"), "mac-studio-07 doesn't offer macOS VMs: it is older than 0.10, or macOS VMs aren't turned on for it."],
  ["MV-N01", NR_MACHINE.OS_MACOS, "macOS"],
  ["MV-N02", NR_MACHINE.OS_WINDOWS_LATER, "Windows virtual machines arrive when your organisation adds a pool that can run them."],
  ["MV-N03", NR_MACHINE.MACOS_ABOUT("macOS 27.0.1 (26A434)"), "macOS 27.0.1 (26A434), set up on the Mac itself from Apple's download. Each Mac runs at most 2."],
  ["MV-N04", NR_MACHINE.MACOS_SIZE("4", "8192"), "CPU and memory start at the macOS minimum, 4 CPU and 8192 MiB. You can raise them on Review."],
  ["MV-N05", NR_MACHINE.MACOS_POOL_NOT_OFFERED, "Every runner in this pool must offer macOS VMs, and at least one doesn't. Choose a pool of Macs that offer them, or ask an admin to make one."],
  ["MV-N06", NR_MACHINE.MACOS_PREPARING("mac-studio-07", 42), "mac-studio-07 is still preparing macOS (42%)."],
  ["MV-N07", NR_MACHINE.MACOS_AT_LIMIT("mac-studio-07"), "mac-studio-07 already runs 2 macOS VMs, the most Apple's licence allows."],
  ["MV-N08", NR_MACHINE.MACOS_NOT_MANAGED_OPERATOR("mac-studio-07", "rnr_7f3a"), "mac-studio-07 is not marked as company-managed. Mark it with `wardyn runner trust rnr_7f3a managed_device`."],
  ["MV-N09", NR_MACHINE.MACOS_NOT_MANAGED_MEMBER("mac-studio-07"), "mac-studio-07 is not marked as company-managed. Ask a security operator to mark it."],
  ["MV-N10", NR_MACHINE.MACOS_NO_POSTURE("mac-studio-07", "/Library/Application Support/Wardyn/posture.json"), "mac-studio-07 has no MDM posture file saying it is managed (`/Library/Application Support/Wardyn/posture.json`)."],
  ["MV-N11", NR_MACHINE.MACOS_HARNESS_ONE("mac-studio-07"), "macOS VMs on mac-studio-07 offer only Your own tools until Apple's Command Line Tools finish installing."],
  ["MV-N12", NR_MACHINE.MACOS_MINIMUM_MARK, "macOS minimum"],
  ["MV-N13", NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"), "A macOS VM needs at least 4 CPU and 8192 MiB."],
  ["MV-N14", NR_MACHINE.MACOS_POOL_CANT_NOW, "Every runner in this pool must be able to take a macOS VM, and at least one can't right now. Try again later, or choose another pool."],
  ["MV-N15", NR_MACHINE.MACOS_HARNESS_POOL("Claude Code"), "Every runner in this pool must offer Claude Code in a macOS VM. Until Apple's Command Line Tools finish installing on each Mac, choose Your own tools."],
];

describe("macOS VM canon", () => {
  it.each(CANON)("%s", (_id, actual, canon) => {
    expect(actual).toBe(canon);
  });
});
