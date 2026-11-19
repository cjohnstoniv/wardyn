/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's window on the Mac (board WIN-1 as approved, and WIN-2's launch line). Ids in the
// comments. The refusals are the server's own sentences (mac_window.message), shown raw; none lives here.

export const MAC_WINDOW = {
  open: (runner: string) => `Open window on ${runner}`, // W-01 as approved
  openCompact: "Open Mac window", // W-01's compact form
  opensOn: (runner: string) => `Opens on ${runner}`, // under the button at every width
  explain: (runner: string) => `Opens a window on ${runner}'s own screen. What you type there goes into this run. Close the window to hand back; the run keeps going.`, // W-02
  waiting: (runner: string) => `Waiting for ${runner} to show the window. This can take up to 30 seconds.`, // W-03
  openChip: "Window open", // W-04
  handBack: (runner: string) => `Close the window on ${runner} to hand back. The run keeps going.`, // W-05
  viewOnlyWhileOpen: "While the window is open, the desktop on this page is view only.", // W-06
  closedPerson: "You closed the window. The run is still going.", // W-08
  closedLocked: (runner: string) => `The window closed because ${runner}'s screen locked. Unlock it and open the window again. The run is still going.`, // W-09
  closedRunEnded: "The window closed because the run ended.", // W-10
  closedFailed: "The window closed unexpectedly. The run is still going. You can open it again.", // W-11
  notOpened: "The window didn't open when the run started.", // W2-12
  background: "This is a background run. What you type in the window goes into it.", // W-12
  watchLine: "In the browser you can watch, not type or click. To work in it, open the window on the Mac.", // the macOS 26 line, under Watch desktop
  /** The Mac's name when the run read carries none. */
  thisMac: "the run's Mac",
} as const;
