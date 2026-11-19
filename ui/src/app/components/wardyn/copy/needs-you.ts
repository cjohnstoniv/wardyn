/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Needs you" (#2963): the run's agent asks a person to take over its desktop or terminal.
// Canon strings, byte-exact from the approved M8 boards (010/mocks-approved/r5-r6-attach-2026-10-20:
// M8-Banner-A, M8-Takeover, M8-HandBack, M8-States). Strings marked "not on the boards" are listed
// in the PR body. Board strings that claimed recording pauses are trimmed or dropped: no recorder
// pauses for a takeover yet, so the copy says nothing about pausing.
export type TakeoverSurface = "desktop" | "terminal";

/** "2 min ago" (M8-Banner-A). Under a minute reads "just now" (not on the boards). */
export function askedAgo(ms: number): string {
  const min = Math.floor(ms / 60_000);
  return min < 1 ? "just now" : `${min} min ago`;
}

/** "5:02", the takeover strip's timer. */
export function heldClock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

export const NEEDS_YOU = {
  /** The run header badge and the approval kind's chip. */
  word: "Needs you",
  banner: {
    title: "The agent needs you",
    // M8-Banner-A (desktop) and M8-Takeover "Before" (a run with no desktop).
    asked: (ago: string, surface: TakeoverSurface) =>
      surface === "terminal"
        ? `Asked ${ago}. Taking over opens the terminal. The agent waits until you take over or decline.`
        : `Asked ${ago}. The agent waits until you take over or decline.`,
    takeOver: "Take over",
    snooze: "Snooze",
    decline: "Decline",
    snooze10: "Snooze for 10 minutes",
    snooze60: "Snooze for 1 hour",
    // M8-States 9.
    snoozedUntil: (time: string) => `Snoozed until ${time}`,
    // UXR-16: a macOS VM run whose desktop is view only in the browser, so a takeover opens the terminal.
    macViewOnly: "This Mac's desktop is view only in the browser. Take over in the terminal, or open a window on your Mac.",
  },
  card: {
    title: "Take over the desktop",
    asked: "What the agent asked:",
    // M8-Banner-A, trimmed: its "Screen recording pauses while you have control." is not true yet.
    blast: "Taking over gives you control of the desktop.",
    requested: (ago: string) => `requested ${ago}`,
  },
  strip: {
    control: (surface: TakeoverSurface) => `You have control of the ${surface}`,
    off: "Recording off",
    // M8-States 10, trimmed: its "Nothing to pause." implied a recorded run pauses.
    offDetail: "This run isn't being recorded.",
    handBack: "Hand back",
    handingBack: "Handing back…",
    // Not on the boards: opens the held takeover's desktop again in a new tab.
    openDesktop: "Open the desktop",
  },
  done: {
    title: "You handed control back",
    // M8-HandBack's first sentence; its recording sentences ("Screen recording resumed at …",
    // "… marked as a gap in the recording.") are dropped as not true yet.
    working: "The agent is working again.",
    audit: "See the audit rows",
    dismiss: "Dismiss",
  },
  // M8-States 3.
  inUse: {
    title: (who: string, time: string) => `${who} took over at ${time}.`,
    detail: (who: string, surface: TakeoverSurface) => `${who} has control of the ${surface} now.`,
  },
  // M8-States 12.
  readOnly: (owner: string) => `The agent is waiting for ${owner}.`,
  toast: {
    title: (run: string) => `${run} needs you`,
    later: "Later",
    // UXR-42: the toast's button only opens the run page, so it says so (a background run's included).
    openRun: "Open run",
    // UXR-25: a hold on the person's own run, raised off the run page. Action: "Review".
    holdTitle: (run: string, host: string) => `${run} is waiting for approval: ${host}`,
    review: "Review",
    browserTitle: (run: string) => `Needs you: ${run}`,
  },
  // Not on the boards: failures of the three verbs, with the server's reason after.
  failed: {
    takeOver: "Couldn't take over",
    // The takeover is held but its desktop did not open.
    openDesktop: "You have control, but the desktop didn't open",
    handBack: "Couldn't hand back",
    decline: "Couldn't decline",
  },
} as const;
