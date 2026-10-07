/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run-detail Output tab (mock packet M8): every string the panel shows.
import { clockTime } from "../../../lib/format";

export const RUN_OUTPUT = {
  tab: "Output",
  eyebrow: "Output",
  sourceStdout: "Command output",
  sourcePane: "Last screen of this session",
  live: "live · refreshing",
  final: "final",
  capturedAt: (t: string) => `Captured ${clockTime(t)}`,
  copyLabel: "Copy output",
  emptyLive: "Nothing printed yet.",
  emptyFinal: "This run printed nothing.",
  globalsOnly:
    "Part of this capture was masked without this run's own secrets, so a secret given to this run may appear unmasked.",
  captureGap:
    "Some of this run's output is missing: the server capturing it restarted, and the rest couldn't be recovered.",
  incomplete: "This capture may be missing its last lines.",
  truncated: "Showing the end only — earlier output wasn't kept.",
  paneCaption:
    "This is the last screen of this session, not a full record. Wardyn captured it just before stopping the run, and the sandbox draws it, so read it as the sandbox's account.",
  loading: "Loading output…",
  cliHint: "Also from the CLI:",
  offTitle: "Run output is off on this deployment",
  // The env var renders mono at the call site: offDescPre + offEnvVar + offDescPost.
  offDescPre: "No run on this server keeps its output — set ",
  offEnvVar: "WARDYN_EXEC_OUTPUT_TAIL=true",
  offDescPost: " to turn it on.",
  savingTitle: "Saving this run's output…",
  savingDesc: "It shows here in a few seconds.",
  notKeptTitle: "No output kept for this run",
  notKeptDesc:
    "Runs that ended before this server kept output, or that it lost track of, have none to show.",
  expiredTitle: "This run's output has been deleted",
  expiredDesc: "This deployment deletes kept output after a set number of days.",
  erasedTitle: "This run's output was erased",
  erasedDesc: "It was removed at a person's erasure request and can't be restored.",
  interactiveTitle: "No last screen to show",
  interactiveDesc:
    "An interactive run keeps its last screen only when Wardyn stops it for you, and only the run's owner or an operator sees it.",
  interactiveNoneDesc:
    "Nothing was kept from this interactive session: it did not end through a Wardyn stop, and recording is off on this deployment.",
  interactiveLiveTitle: "No last screen yet",
  interactiveLiveDesc: "This session is still open. Wardyn keeps its last screen if it stops the run.",
  interactiveLink: "Open the Recording tab →",
  notCapturedTitle: "Output isn't captured for Kubernetes runs yet",
  notCapturedDesc: "The run's recording has it.",
  maskUnavailable:
    "This server can't show the live output safely right now. It shows here once the run ends.",
} as const;
