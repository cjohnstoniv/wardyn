/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's window on your computer, for a Linux VM run. The refusals are the server's own sentences
// (local_window.message), shown raw; none lives here. The open state reuses the Mac's words (MAC_WINDOW).

export const WINDOW_LOCAL = {
  TITLE: "Window on your computer",
  NOTE: "Opens this run's desktop in a window on your own computer. Run it on the Linux computer this run's runner is on. Copy and paste don't reach it.",
  COMMAND: (runId: string) => `wardyn window ${runId}`,
  OPEN_SINCE: (runner: string, time: string) => `A window has been open on ${runner} since ${time}.`,
  /** The runner's name when the run read carries none. */
  RUNNER_FALLBACK: "this run's runner",
} as const;
