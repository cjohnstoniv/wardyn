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
  BELOW: "Profiles and policies below may record more, never less.",
  // A profile ceiling only: what a level does to a macOS VM run (no level refuses one at create; a required recording that can't be made stops it at start).
  MACOS_VM_CEILING:
    "Choosing a recording level doesn't by itself rule out macOS VMs. At Terminal a macOS VM's desktop isn't recorded; at Screen and actions it is, unmasked, while someone watches. A task run's agent output is recorded when output capture is on; if recording is required and that can't be done, the run doesn't start.",
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
  // At Screen and actions, on a run that has a screen: what the screen recording keeps.
  SCREEN_NOTE: "The screen is kept while someone watches it. It isn't masked.",
  // A macOS VM run marked runner_declined: only its agent's output went unrecorded; the level's own lines still hold.
  AGENT_NOT_RECORDED: "The agent's output isn't recorded.",
  OFF: "Recording off",
  OFF_NOTE: "Nothing is captured. The audit log still shows what happened.",
  UNAVAILABLE: "Recording unavailable",
} as const;

// The desktop tile's line at Screen and actions, and what it says while the agent is paused for a sign-in
// (no frames are kept then).
export const RECORDING_DESKTOP = {
  WATCHED: "Recorded while you watch; not masked",
  PAUSED: "Recording paused while you sign in.",
} as const;

// The player's footer on a macOS VM run: its recordings are the organisation's, not its runner's.
export const RECORDING_FOOTER_MACOS = "Recorded at this run's recording level.";

// One line under the session picker for each way the picked recording is not whole.
export const RECORDING_COMPLETENESS = {
  READ_ERROR: "The runner's copy ended early; the rest of the output is missing.",
  MASK_GAP: "Some output was left out because Wardyn couldn't check it for secrets.",
  TRUNCATED: (limit: string) => `Only the first ${limit} was kept.`,
  GAP_BYTES: "The start of the output is missing; the runner had already discarded it.",
  UNKNOWN: "Wardyn couldn't confirm this copy is complete.",
} as const;

/** Whether the run's screen is being recorded: Screen and actions, and not marked unrecorded (on a macOS VM run the runner_declined mark is about the agent's output only). */
export function desktopRecordedAt(run: { recording_level?: RecordingLevel; not_recorded?: string; machine_kind?: string }): boolean {
  return run.recording_level === "screen_actions" && (!run.not_recorded || (run.machine_kind === "macos_vm" && run.not_recorded === "runner_declined"));
}

export const RECORDING_REPLAY = {
  SCREEN: "Screen and actions",
  TERMINAL: "Terminal",
  DOWNLOAD: "Download recording",
  PAUSED_FOR: (d: string) => `Recording paused for ${d}`,
  // The reason is Wardyn's, e.g. "you took over to sign in".
  PAUSED_BODY: (reason: string) =>
    `${reason.charAt(0).toUpperCase()}${reason.slice(1)}, so no screen frames were kept. The action log kept going. The gap is marked in the audit log.`,
  GAP: (d: string) => `Gap ${d}`,
  // Pending the owner's approval of the copy: the recording hit its size cap or the stream stopped being followable.
  STOPPED_AT: (time: string) => `Screen recording stopped at ${time}; the action log continues.`,
  KEYSTROKES: "Keystrokes are never recorded.",
  LOG_TITLE: "Tamper-evident log",
  GAP_START: "Gap start",
  GAP_END: "Gap end",
  REASON: "Reason",
  WRITTEN_BY: "Written by",
  WRITTEN_BY_VALUE: "Wardyn, not the desktop",
  // Not on the boards: the decoder could not load, so only the download is left.
  SCREEN_FAILED: "The screen could not be drawn here. Download the recording to watch it in noVNC's playback page.",
  // Not on the boards: the picker's row for a desktop recording.
  SESSION_OPTION: (ended: string) => `Screen and actions · ended ${ended}`,
  LOADING: "Loading the captured session…",
  LOAD_FAILED: "Couldn't load this run's recording.",
  LOAD_FAILED_NOTE: "The action log is still in Audit. A failed load says nothing about whether a recording exists.",
  EMPTY_TITLE: "No recording available",
  // M7-States 8, with the run's policy in the walls' place.
  OFF_BY_POLICY: "This run's policy set recording to Off. The audit log still shows what happened.",
  // The player (M7-Replay).
  SKIP_GAP: "Skip the gap",
  SHOW_GAP: "Show it on the timeline",
  SPEED: "Playback speed",
  LEGEND_ACTION: "Action",
  LEGEND_BLOCKED: "Clipboard or file blocked",
  LEGEND_PAUSED: "Paused",
  ACTION_LOG: "Action log",
  ACTION_LOG_HINT: "Click a row to jump there",
  SHOWING: (n: number, m: number) => `Showing ${n} of ${m} rows`,
  OPEN_AUDIT: "Open in Audit",
  ACT_PASTE: "pasted into the desktop",
  ACT_COPY_TRIED: "tried to copy out of the desktop",
  ACT_UPLOAD: "uploaded a file to the desktop",
  ACT_PAUSED: "paused the screen recording",
  ACT_PAUSED_DETAIL: (reason: string) => `reason: ${reason}`,
  ACT_RESUMED: "resumed the screen recording",
  ACT_RESUMED_DETAIL: (d: string) => `gap ${d}`,
  CHAIN_OK: "Chain intact",
  CHAIN_RANGE: (first: number, last: number) =>
    `Entries ${first.toLocaleString("en-US")} to ${last.toLocaleString("en-US")} are chained and none is missing.`,
  ENTRY_AT: (seq: number, time: string) => `Entry ${seq.toLocaleString("en-US")} · ${time}`,
  // Not on the boards: the other directions and outcomes of the same rows.
  ACT_PASTE_TRIED: "tried to paste into the desktop",
  ACT_COPY: "copied out of the desktop",
  ACT_UPLOAD_TRIED: "tried to upload a file to the desktop",
  ACT_DOWNLOAD: "downloaded a file from the desktop",
  ACT_DOWNLOAD_TRIED: "tried to download a file from the desktop",
  // Not on the boards: the controls' names, and the chain check's other answers.
  PLAY: "Play",
  PAUSE: "Pause",
  CHAIN_CHECKING: "Checking the chain…",
  CHAIN_BROKEN: "Chain broken",
  CHAIN_BROKEN_BODY: (seq: number, reason: string) => `Entry ${seq.toLocaleString("en-US")} does not verify: ${reason}.`,
  CHAIN_UNAVAILABLE: "The chain could not be checked for this recording.",
} as const;

/** "2 min 14 s", the boards' duration form. */
export function gapDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(s / 60);
  return m > 0 ? `${m} min ${s % 60} s` : `${s} s`;
}
