/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Needs you" (#2963): the run's agent asks a person to take over its desktop or terminal.
// Canon strings, byte-exact from the approved M8 boards (010/mocks-approved/r5-r6-attach-2026-10-20:
// M8-Banner-A, M8-Takeover, M8-HandBack, M8-States). Strings marked "not on the boards" are listed
// in the PR body.
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

/** "5 min 02 s", the hand-back confirmation's length of the gap. */
export function heldSpan(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, "0")} s`;
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
  },
  card: {
    title: "Take over the desktop",
    asked: "What the agent asked:",
    blast: "Taking over gives you control of the desktop. Screen recording pauses while you have control.",
    requested: (ago: string) => `requested ${ago}`,
  },
  strip: {
    control: (surface: TakeoverSurface) => `You have control of the ${surface}`,
    paused: "Recording paused",
    off: "Recording off",
    // M8-States 10.
    offDetail: "Nothing to pause. This run isn't being recorded.",
    handBack: "Hand back",
    handingBack: "Handing back…",
  },
  done: {
    title: "You handed control back",
    resumed: (time: string) => `The agent is working again. Screen recording resumed at ${time}.`,
    gap: (span: string) => `The ${span} you were in control is marked as a gap in the recording.`,
    // Not on the boards: the same confirmation on a run with no recording.
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
    browserTitle: (run: string) => `Needs you: ${run}`,
  },
  // Not on the boards: failures of the three verbs, with the server's reason after.
  failed: {
    takeOver: "Couldn't take over",
    handBack: "Couldn't hand back",
    decline: "Couldn't decline",
  },
} as const;
