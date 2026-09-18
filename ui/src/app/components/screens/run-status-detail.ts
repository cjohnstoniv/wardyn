/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WHAT A STARTING RUN IS WAITING ON (0.7.5 field report, finding 6).
//
// The server sends the SUBSTRATE's own line — `<component>: <Reason>[: <message>]`
// — plus the bare reason token it derived from it (`status_reason`,
// internal/api/runs_status_detail.go). This module owns the COPY: the server
// never composes a sentence, because the same reason has to read differently on
// a header chip, in a board row and in the sign-in pane, and because a reason
// Wardyn has no sentence for must still degrade to something honest.
//
// NO CSS, NO COMPONENT IMPORTS, ON PURPOSE. The run header, the Runs board, the
// sign-in pane AND ui/e2e's live specs all read these constants; a Playwright
// spec runs in Node and cannot load `xterm.css`, which is what any import path
// through the pane drags in. Keep this file dependency-free.

// TERMINAL_STATUS_REASONS mirrors internal/runner/waiting.go's
// TerminalWaitingReasons — the reasons waiting cannot fix, where only a person
// (a corrected reference, a registry credential, a working entrypoint) changes
// the answer. A hand-maintained mirror, like every other TS view of a Go type in
// this console; run-status-detail.test.ts reads that Go file and fails if the
// two lists drift, which is the one cheap guard available.
export const TERMINAL_STATUS_REASONS: readonly string[] = [
  "ImagePullBackOff",
  "ErrImagePull",
  "CreateContainerError",
  "CreateContainerConfigError",
  "InvalidImageName",
  "CrashLoopBackOff",
];

// isTerminalStatusReason: waiting longer will not change this answer. The empty
// reason is NOT terminal — "we have not read one" is not a verdict.
export function isTerminalStatusReason(reason: string | null | undefined): boolean {
  return !!reason && TERMINAL_STATUS_REASONS.includes(reason);
}

// ─── DRAFT strings (M2 canon pending) ────────────────────────────────────────
// One block, one file. Every test asserts THROUGH these constants.

// DRAFT (M2 canon pending) — the ordinary wait. The kubelet reports
// ContainerCreating for a pull and for everything else it does before a
// container runs (canary.go's own comment), so the pull is named as the usual
// CAUSE, conditionally, never as the diagnosis — the hedge login-pane-copy.ts's
// U-12 note settled on.
export const STARTING_CONTAINER_CREATING =
  "Starting the sandbox. The first start after an update can take a couple of minutes while the image downloads."; // UX round S9: no k8s nouns for a member; still conditional
// DRAFT (M2 canon pending) — docker only, the one place a FIRST PULL can
// honestly be ASSERTED (ensureImage).
export const STARTING_FIRST_PULL =
  "Downloading the image. The first start after an update takes a couple of minutes."; // Codex #11: imagePresent=false proves the image is not cached NOW — pruning invalidates any "never ran here" claim
// DRAFT (M2 canon pending) — the pod exists but nothing will take it. Not terminal.
export const STARTING_UNSCHEDULABLE = "Waiting for a machine with room for this sandbox.";
// DRAFT (M2 canon pending) — Pending with no container status at all.
export const STARTING_WAITING_FOR_NODE = "Waiting for a machine to start it on.";
// DRAFT (M2 canon pending) — TERMINAL; the registry's own words follow the colon
// because they name the fix.
export const STUCK_IMAGE_PULL = "The image could not be pulled:";
// DRAFT (M2 canon pending) — TERMINAL, and nothing about the cluster will change it.
export const STUCK_IMAGE_NAME = "That image reference is not valid:";
// DRAFT (M2 canon pending) — TERMINAL. The image exists; the kubelet would not
// make a container from it.
export const STUCK_CREATE_CONTAINER = "The sandbox container could not be created:";
// DRAFT (M2 canon pending) — TERMINAL for a START: the container starts and
// exits, repeatedly.
export const STUCK_CRASH_LOOP = "The sandbox container keeps exiting as it starts:";
// DRAFT (M2 canon pending) — the lead-in for a reason this console has no
// sentence for. Prefixed so a bare `pod: SomeReason: msg` never LEADS: the same
// honest degradation failure_hint already has, with a word in front of it
// saying that the rest is the platform talking.
export const STARTING_RAW_PREFIX = "Waiting: ";

// The SHORT register (round-2 UX S9). The header chip is `max-w-[160px]`, so the
// sentences above truncate to a restatement of the STARTING badge — "Starting
// the sandbo…" — and the registry's words, the whole point of a terminal
// reason, never appear at all. These are ~20 characters and say the one thing
// the badge does not.
export const CHIP_DOWNLOADING = "Downloading the image";
export const CHIP_SETTING_UP = "Setting up the container"; // lane-added: ContainerCreating/PodInitializing are the commonest state and "Starting the sandbox" would only restate the badge
export const CHIP_WAITING_FOR_MACHINE = "Waiting for a machine";
export const CHIP_IMAGE_PULL_FAILED = "Image pull failed";
export const CHIP_BAD_IMAGE_REF = "Bad image reference"; // lane-added: nothing was pulled for InvalidImageName, so "Image pull failed" would be false
export const CHIP_CONTAINER_WONT_START = "Container won't start";

// ─── parsing ─────────────────────────────────────────────────────────────────

export type ParsedStatusDetail = {
  // "agent", "proxy", "pod", "image" — which part of the substrate is talking.
  component: string;
  // The bare reason token: "ContainerCreating", "ImagePullBackOff", "Pulling"…
  reason: string;
  // The platform's own message, or "". Routinely contains colons.
  message: string;
};

// parseStatusDetail splits the raw line on its FIRST TWO colons: a component
// name and a substrate reason never contain one, and everything after the second
// is the platform's message, which very often does ("rpc error: code = Unknown
// desc = …").
//
// `reason` (the server's derived status_reason) WINS when present — the server
// did the same split and is the authority on it. The string is parsed only for a
// pre-0.7.6 daemon, which sends the detail and no token.
//
// ponytail: two `split`s and no grammar. A substrate line that is not in this
// shape yields an empty reason, which routes to the raw-prefixed sentence — the
// honest degradation, not a parse error.
export function parseStatusDetail(
  raw: string | null | undefined,
  reason?: string | null,
): ParsedStatusDetail {
  const text = (raw ?? "").trim();
  if (!text) return { component: "", reason: reason?.trim() ?? "", message: "" };
  const first = text.indexOf(": ");
  if (first < 0) return { component: "", reason: reason?.trim() ?? "", message: "" };
  const rest = text.slice(first + 2);
  const second = rest.indexOf(": ");
  const parsed = second < 0
    ? { component: text.slice(0, first), reason: rest.trim(), message: "" }
    : { component: text.slice(0, first), reason: rest.slice(0, second).trim(), message: rest.slice(second + 2).trim() };
  return reason?.trim() ? { ...parsed, reason: reason.trim() } : parsed;
}

// statusDetailSentence is what a person reads. "" when there is nothing to say,
// so every caller can render it unconditionally.
export function statusDetailSentence(raw: string | null | undefined, reason?: string | null): string {
  const text = (raw ?? "").trim();
  if (!text) return "";
  const d = parseStatusDetail(text, reason);
  const withMessage = (lead: string) => (d.message ? `${lead} ${d.message}` : lead);
  switch (d.reason) {
    case "ContainerCreating":
    case "PodInitializing":
      return STARTING_CONTAINER_CREATING;
    case "Pulling":
      return STARTING_FIRST_PULL;
    case "Unschedulable":
      return STARTING_UNSCHEDULABLE;
    // An empty reason belongs here rather than with the unknown ones below: the
    // line parsed, it just named no reason, which is the same "the pod is there
    // and nothing has taken it" fact Pending states.
    case "Pending":
    case "":
      return STARTING_WAITING_FOR_NODE;
    case "ImagePullBackOff":
    case "ErrImagePull":
      return withMessage(STUCK_IMAGE_PULL);
    case "InvalidImageName":
      return withMessage(STUCK_IMAGE_NAME);
    case "CreateContainerError":
    case "CreateContainerConfigError":
      return withMessage(STUCK_CREATE_CONTAINER);
    case "CrashLoopBackOff":
      return withMessage(STUCK_CRASH_LOOP);
    default:
      // A reason this console has no sentence for: hand the platform's own line
      // over, prefixed so it never LEADS as if Wardyn had said it.
      return STARTING_RAW_PREFIX + text;
  }
}

// statusDetailChip is the SAME facts in the header's ~20-character register.
// "" when there is nothing to say.
export function statusDetailChip(raw: string | null | undefined, reason?: string | null): string {
  const text = (raw ?? "").trim();
  if (!text) return "";
  switch (parseStatusDetail(text, reason).reason) {
    case "Pulling":
      return CHIP_DOWNLOADING;
    case "ContainerCreating":
    case "PodInitializing":
      return CHIP_SETTING_UP;
    case "ImagePullBackOff":
    case "ErrImagePull":
      return CHIP_IMAGE_PULL_FAILED;
    case "InvalidImageName":
      return CHIP_BAD_IMAGE_REF;
    case "CreateContainerError":
    case "CreateContainerConfigError":
    case "CrashLoopBackOff":
      return CHIP_CONTAINER_WONT_START;
    default:
      // Unschedulable, Pending, an empty reason, and anything unknown: the chip
      // says the general fact and its `title` carries the full sentence.
      return CHIP_WAITING_FOR_MACHINE;
  }
}
