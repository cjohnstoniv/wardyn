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
} as const;
