/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Open desktop (mock M6: M6-RunDetail, M6-Desktop, M6-States, M6-Phone). Byte for byte from the boards.
// The button itself is UI_APPS_LANE.cta("desktop"), so the "Open …" pattern holds.

export const DESKTOP = {
  title: "Desktop",
  ready: "Ready",
  statusOff: "Off",
  kind: "Linux desktop VM",
  off: "Off on this deployment. It relays the desktop of a sandbox to your browser through Wardyn. An operator turns it on by setting WARDYN_UI_SANDBOX_LISTEN where wardynd starts.",
  phoneBefore: "Opens view only on a small screen. Use a larger screen to type and click.",
  // The viewer, on the gateway's own address.
  connected: "Connected",
  fit: "Fit",
  actualSize: "Actual size",
  close: "Close",
  // Not on the boards: the gateway did not store the Close (503), so the session is still valid.
  closeFailed: "Close didn't go through, so this desktop is still open. Try again.",
  starting: "Starting the desktop",
  startingBody: "This tab connects on its own. You don't need to reload it.",
  phoneTitle: "Small screen: view only",
  phoneBody: "Use a larger screen to type and click.",
  sessionEnded: "This desktop session ended",
  sessionEndedBody: "It ran for 1 hour, the longest one session lasts. The run is still going. Open the desktop again from the run page.",
  runEnded: "This run has ended. The desktop closed with it.",
  // Not on the boards: the gateway refused this tab's session (403 on the socket), so it stops retrying.
  refusedBody: "Wardyn no longer accepts this tab's session. Open the desktop again from the run page.",
  // A link that no longer opens anything (the approved Why card shape).
  linkBlocked: "Blocked: this desktop link",
  linkRefused: "Refused at the gateway",
  linkBody: "A desktop link works once and only for a short time. This one was used already, or it ran out of time.",
  whyWall: "Wall",
  whyWallBody: "No wall. A desktop link works once.",
  whySetBy: "Set by",
  whySetByBody: "Wardyn",
  whyEnforced: "Enforced",
  whyEnforcedBody: "At the UI gateway, before the desktop is reached",
  whyCanChange: "Can change",
  whyCanChangeBody: "Yes. Open the desktop again from the run page to get a new link.",
  gatewaySaid: "What the gateway said:",
  copyDetails: "Copy details",
  // Sign-ins in a desktop (#2972, board R11-2972-DesktopSignIns). Byte for byte from the board.
  signInsNote:
    "Sign-ins you make in this desktop are kept in this machine's memory and are gone when it shuts down: at the run's end, or when the run is killed. While the run goes on, the agent can use them too. Closing this tab doesn't remove them. Anything you save to a file stays with the run's files, where the agent can read it.",
  pauseToSignIn: "Pause the agent to sign in",
  agentPaused: "Agent paused",
  pausedNote: "The agent is paused while you sign in. Once you resume it, it can use what you signed into.",
  resume: "Resume the agent",
  signInsOff: (agent: string) => `Your admin doesn't allow sign-ins in this desktop while ${agent} runs, so this desktop keeps none.`,
  // macOS virtual machines (MV-D*): the approved canon, ids in the comments.
  kindMacOS: "macOS desktop VM", // MV-D01
  signInsNoteRunDisk:
    "Sign-ins you make in this desktop are kept on this run's virtual disk and removed from this Mac when the run ends. The Mac's own snapshots or backups may keep a copy for a while. While the run goes on, the agent can use them too. Closing this tab doesn't remove them.", // MV-D08
  observeOnlyMacOS: "The macOS desktop isn't available on this Mac yet: macOS turned Screen Sharing on for viewing only.", // MV-D09
  diskRemoving: "Removing the macOS VM's disk from this Mac…", // MV-D10
  diskRemoved: (time: string) => `The macOS VM and its disk were removed from this Mac at ${time}.`, // MV-D11
  diskRemoveFailed: (reason: string) => `The macOS VM's disk could not be removed from this Mac: ${reason}.`, // MV-D12
  // MV-D12's <reason>, from the runner's closed code; MV-D12c for an empty or unknown one.
  diskReasonInUse: "the VM's files are still in use", // MV-D12a
  diskReasonDeleteFailed: "macOS couldn't delete its files", // MV-D12b
  diskReasonUnknown: "the Mac didn't say why", // MV-D12c
  endedLowDisk: (gb: string) => `This Mac ran low on disk (${gb} GB free), so the macOS VM was stopped.`, // MV-D13
  endedRunnerUpgraded: "This Mac's runner was upgraded while the macOS VM ran, so the run was ended.", // MV-D14
  startingMacOS: "The macOS desktop is still starting.", // MV-D15
} as const;
