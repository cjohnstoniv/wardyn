/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Recording levels (#2968), byte-exact from the approved M7 boards (Knobs-B, RunStrip, Replay,
// States). Strings the boards do not carry are marked "not on the boards" and listed in the PR.
import type { RecordingLevel } from "../../../lib/types/policy";

export const RECORDING_LEVEL = {
  ROW: "Recording",
  LEVELS: [
    {
      value: "off",
      label: "Off",
      description: "No screen or terminal is recorded. The audit log still shows what happened.",
    },
    { value: "terminal", label: "Terminal", description: "Records terminal sessions only. No screen." },
    {
      value: "screen_actions",
      label: "Screen and actions",
      description: "Records the screen and an action log of what was opened, copied and transferred.",
    },
  ] as const satisfies readonly { value: RecordingLevel; label: string; description: string }[],
  KEYSTROKES: "Keystrokes",
  KEYSTROKES_VALUE: "Never recorded",
  KEYSTROKES_NOTE: "Not a setting. No wall, team or run can turn it on.",
  BELOW: "Teams below may record more, never less.",
  LOCKED: "Locked",
  // M7-States 3, with the organisation in the walls' place (the walls are not on this release).
  MINIMUM_ABOVE: (who: string, level: string, lower: string[]) =>
    `${who} requires at least ${level}. ${lower.join(" and ")} ${lower.length > 1 ? "aren't" : "isn't"} available here.`,
  // Not on the boards: who sets the minimum when it is the organisation's own.
  ORG: "Your organisation",
  // Not on the boards: the policy editor's note when nothing is chosen.
  UNSET: "Not set: the organisation's level applies.",
  DISABLED: "Disabled",
} as const;

export function recordingLevelLabel(l: RecordingLevel | undefined): string {
  return RECORDING_LEVEL.LEVELS.find((o) => o.value === (l ?? "terminal"))!.label;
}

export const RECORDING_STRIP = {
  ON: (level: string) => `Recording · ${level}`,
  TERMINAL_ONLY: "Recording · Terminal only",
  TERMINAL_ONLY_NOTE: "The screen is not recorded.",
  OFF: "Recording off",
  OFF_NOTE: "Nothing is captured. The audit log still shows what happened.",
  UNAVAILABLE: "Recording unavailable",
} as const;

export const RECORDING_REPLAY = {
  SCREEN: "Screen and actions",
  TERMINAL: "Terminal",
  DOWNLOAD: "Download recording",
  PAUSED_FOR: (d: string) => `Recording paused for ${d}`,
  // The reason is Wardyn's, e.g. "you took over to sign in".
  PAUSED_BODY: (reason: string) =>
    `${reason.charAt(0).toUpperCase()}${reason.slice(1)}, so no screen frames were kept. The action log kept going. The gap is marked in the audit log.`,
  GAP: (d: string) => `Gap ${d}`,
  KEYSTROKES: "Keystrokes are never recorded.",
  LOG_TITLE: "Tamper-evident log",
  GAP_START: "Gap start",
  GAP_END: "Gap end",
  REASON: "Reason",
  WRITTEN_BY: "Written by",
  WRITTEN_BY_VALUE: "Wardyn, not the desktop",
  // Not on the boards: the console has no screen player yet.
  PLAYER_PENDING: "Download the recording to watch it in noVNC's playback page.",
  // Not on the boards: the picker's row for a desktop recording.
  SESSION_OPTION: (ended: string) => `Screen and actions · ended ${ended}`,
  LOADING: "Loading the captured session…",
  LOAD_FAILED: "Couldn't load this run's recording.",
  LOAD_FAILED_NOTE: "The action log is still in Audit. A failed load says nothing about whether a recording exists.",
  EMPTY_TITLE: "No recording available",
  // M7-States 8, with the run's policy in the walls' place.
  OFF_BY_POLICY: "This run's policy set recording to Off. The audit log still shows what happened.",
} as const;

/** "2 min 14 s", the boards' duration form. */
export function gapDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(s / 60);
  return m > 0 ? `${m} min ${s % 60} s` : `${s} s`;
}
