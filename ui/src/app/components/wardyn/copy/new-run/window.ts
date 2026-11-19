/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's window on the Mac (board WIN-2), and the OS row's lines that depend on it. Byte for byte from the board,
// ids in the comments. Lazy-only, like the other New Run copy modules.

export const NR_WINDOW = {
  GROUP: "Window", // W2-01
  OPTION: "Open in a window on my Mac", // W2-02
  ONE_MAC: (runner: string) => `Opens a window on ${runner} as the run starts, where you can type and click. You can also open one later from the run page.`, // W2-03
  BE_THERE: (runner: string) => `Be at ${runner} with its screen unlocked when the run starts. If the window can't open, the run still starts and its page says why.`, // W2-04
  SEVERAL_MACS: "Opens only if the run lands on a Mac you registered. On any other Mac the run starts without a window, and its page says why.", // W2-05
  CLEARED: "Window choice cleared.", // W2-06
  REVIEW_ONE_MAC: (runner: string) => `Window on ${runner} when the run starts`, // W2-07
  REVIEW_SEVERAL_MACS: "Window on your Mac when the run starts, if it lands on one you registered", // W2-08
  // The OS row's lines. They stand in for the machine copy's own while the window decides what they say.
  OS_WINDOWS_LATER: "Windows virtual machines aren't available yet.", // V-05R
  OS_LATER: "macOS virtual machines are turned off in this organisation. Windows virtual machines aren't available yet.", // V-06R
  VIEW_ONLY_WINDOW_ON: "On macOS 26 the browser shows this desktop view only. Use the terminal for commands, or open a window on the Mac to interact with the desktop.", // E-02, switch on
  VIEW_ONLY_WINDOW_OFF: "On macOS 26 the browser shows this desktop view only. Use the terminal for commands.", // E-02, switch off
} as const;
