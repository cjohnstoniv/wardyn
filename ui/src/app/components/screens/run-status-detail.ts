/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a starting run is waiting on.
//
// The server sends the substrate's own line — `<component>: <Reason>[: <message>]`
// — plus the bare reason token it derived from it (`status_reason`,
// internal/api/runs_status_detail.go). This module owns the copy: the server
// never composes a sentence, because the same reason has to read differently on
// a header chip, in a board row and in the sign-in pane, and because a reason
// Wardyn has no sentence for must still degrade to something honest.
//
// No CSS, no component imports, on purpose. The run header, the Runs board, the
// sign-in pane and ui/e2e's live specs all read these constants; a Playwright
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

// Draft strings (M2 canon pending)
// One block, one file. Every test asserts through these constants.

// DRAFT (M2 canon pending) — the ordinary wait. The kubelet reports
// ContainerCreating for a pull and for everything else it does before a
// container runs (canary.go's own comment), so the pull is named as the usual
// cause, conditionally, never as the diagnosis — the hedge login-pane-copy.ts's
// U-12 note settled on.
export const STARTING_CONTAINER_CREATING =
  "Starting the sandbox. The first start after an update can take a couple of minutes while the image downloads."; // no k8s nouns for a member; still conditional
// DRAFT (M2 canon pending) — docker only, the one place a first pull can
// honestly be asserted (ensureImage).
export const STARTING_FIRST_PULL =
  "Downloading the image. The first start after an update takes a couple of minutes."; // Codex #11: imagePresent=false proves the image is not cached now — pruning invalidates any "never ran here" claim
// DRAFT (M2 canon pending) — the pod exists but nothing will take it. Not terminal.
export const STARTING_UNSCHEDULABLE = "Waiting for a machine with room for this sandbox.";
// DRAFT (M2 canon pending) — Pending with no container status at all.
export const STARTING_WAITING_FOR_NODE = "Waiting for a machine to start it on.";
// DRAFT (M2 canon pending) — terminal; the registry's own words follow the colon
// because they name the fix.
export const STUCK_IMAGE_PULL = "The image could not be pulled:";
// DRAFT (M2 canon pending) — terminal, and nothing about the cluster will change it.
export const STUCK_IMAGE_NAME = "That image reference is not valid:";
// DRAFT (M2 canon pending) — terminal. The image exists; the kubelet would not
// make a container from it.
export const STUCK_CREATE_CONTAINER = "The sandbox container could not be created:";
// DRAFT (M2 canon pending) — terminal for a start: the container starts and
// exits, repeatedly.
export const STUCK_CRASH_LOOP = "The sandbox container keeps exiting as it starts:";
// DRAFT (M2 canon pending) — the lead-in for a reason this console has no
// sentence for. Prefixed so a bare `pod: SomeReason: msg` never leads: the same
// honest degradation failure_hint already has, with a word in front of it
// saying that the rest is the platform talking.
export const STARTING_RAW_PREFIX = "Waiting: ";

// #125 (Q125-2, canon) — PENDING's own first tick: the substrate has not sent
// anything yet, so the ordinary derivation above (keyed on status_reason)
// would render nothing at all. STARTING's own empty-detail case is correctly
// silent — there is no ordinary wait worth naming before the pod is even
// scheduled — but PENDING is earlier still, and an all-blank header there
// reads as "nothing is happening" rather than "the request is in flight".
// Superseded the instant a real stage line lands: run-detail-summary-header.tsx
// only reaches for this sentence while status_detail is still empty.
export const PENDING_NO_DETAIL = "Queued — Wardyn is getting this run ready.";

// The short register. The header chip is `max-w-[160px]`, so the
// sentences above truncate to a restatement of the starting badge — "Starting
// the sandbo…" — and the registry's words, the whole point of a terminal
// reason, never appear at all. These are ~20 characters and say the one thing
// the badge does not.
export const CHIP_DOWNLOADING = "Downloading the image";
export const CHIP_SETTING_UP = "Setting up the container"; // ContainerCreating/PodInitializing are the commonest state and "Starting the sandbox" would only restate the badge
export const CHIP_WAITING_FOR_MACHINE = "Waiting for a machine";
export const CHIP_IMAGE_PULL_FAILED = "Image pull failed";
export const CHIP_BAD_IMAGE_REF = "Bad image reference"; // nothing was pulled for InvalidImageName, so "Image pull failed" would be false
export const CHIP_CONTAINER_WONT_START = "Container won't start";

// Parsing

export type ParsedStatusDetail = {
  // "agent", "proxy", "pod", "image" — which part of the substrate is talking.
  component: string;
  // The bare reason token: "ContainerCreating", "ImagePullBackOff", "Pulling"…
  reason: string;
  // The platform's own message, or "". Routinely contains colons.
  message: string;
};

// parseStatusDetail splits the raw line on its first two colons: a component
// name and a substrate reason never contain one, and everything after the second
// is the platform's message, which very often does ("rpc error: code = Unknown
// desc = …").
//
// `reason` (the server's derived status_reason) wins when present — the server
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
  const d = parseStatusDetail(text, reason);
  if (!text) {
    // Defence in depth: the server rebuilds the detail from failure_hint
    // whenever it has a terminal reason, so a terminal reason with no words
    // should be unreachable — and if it ever is reached, the pane has already
    // promised the reader a sentence after its lead-in, so an empty string is
    // the one answer that must not come back.
    return isTerminalStatusReason(d.reason) ? STARTING_RAW_PREFIX + d.reason : "";
  }
  const withMessage = (lead: string) => (d.message ? `${lead} ${d.message}` : lead);
  switch (d.reason) {
    case "ContainerCreating":
    case "PodInitializing":
      return STARTING_CONTAINER_CREATING;
    case "Pulling":
      return STARTING_FIRST_PULL;
    case "Unschedulable":
      return STARTING_UNSCHEDULABLE;
    case "Pending":
      return STARTING_WAITING_FOR_NODE;
    // An empty reason on a line that parsed (it named a component) is the same
    // "the pod is there and nothing has taken it" fact Pending states. A line
    // that did not parse at all is not — calling an unrecognised
    // string a node wait invents exactly the kind of diagnosis this module
    // exists to stop, so it falls through to the raw arm below.
    case "":
      if (d.component) return STARTING_WAITING_FOR_NODE;
      return STARTING_RAW_PREFIX + text;
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
      // over, prefixed so it never leads as if Wardyn had said it.
      return STARTING_RAW_PREFIX + text;
  }
}

// statusDetailChip is the same facts in the header's ~20-character register.
// "" when there is nothing to say.
export function statusDetailChip(raw: string | null | undefined, reason?: string | null): string {
  const text = (raw ?? "").trim();
  const d = parseStatusDetail(text, reason);
  // Same defence as the sentence's: a terminal reason always gets a
  // chip, detail or no detail.
  if (!text && !isTerminalStatusReason(d.reason)) return "";
  switch (d.reason) {
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
    case "Unschedulable":
    case "Pending":
      return CHIP_WAITING_FOR_MACHINE;
    default:
      // An unknown reason is not a machine wait. The chip is the
      // only register a narrow header ever shows, so asserting the wrong short
      // answer there is worse than carrying a long true one; the `title` has the
      // full sentence either way. An absent reason keeps the general chip.
      return d.reason ? STARTING_RAW_PREFIX + d.reason : CHIP_WAITING_FOR_MACHINE;
  }
}
