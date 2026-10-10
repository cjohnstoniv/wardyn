/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run-mode refusals the server and the console say in the same bytes: the
// wire reasons and the sentences. The console prints the server's sentence on a
// refusal and uses these for its own pre-check, so the two never disagree. Both
// sides are pinned to run-mode-refusals.golden.json (internal/api/run_mode_test.go,
// run-mode-refusals.test.ts); change a sentence in run_mode_refusals.go, here and
// in that table together.

/** The wire reasons the run-mode contract answers with (internal/api/reasons.go). */
export const RUN_MODE_REASON = {
  /** A new client's mode or a background task's workload is unset, and is never inferred. */
  REQUIRED: "run_mode_required",
  INVALID: "run_mode_invalid",
  /** Carriers contradict each other, or an older field that replaced them. */
  CONFLICT: "run_mode_conflict",
  START_FOLDER_INVALID: "start_folder_invalid",
  /** An interactive door of a background run. */
  BACKGROUND_ONLY: "run_background_only",
} as const;

export type RunModeReason = (typeof RUN_MODE_REASON)[keyof typeof RUN_MODE_REASON];

/** The sentences, byte for byte what internal/api/run_mode_refusals.go says. */
export const RUN_MODE_REFUSAL = {
  RUN_MODE_REQUIRED: () => "Choose what you're starting: a background task or an interactive environment.",
  RUN_WORKLOAD_REQUIRED: () => "A background task needs something to run. Choose an agent task or a command.",
  RUN_MODE_CONFLICT: (field: string, instead: string) => `${field} cannot be sent with the run-mode fields. ${instead}`,
  STARTUP_TOOL_UNKNOWN: (tool: string) =>
    `The startup names ${tool}, which this run does not include. Include it as a tool or choose another startup.`,
  START_FOLDER_ATTACHMENT: (attachment: string) => `${attachment} is not attached to this run, so it cannot hold the starting folder.`,
  START_FOLDER_SHAPE: (subpath: string) => `${subpath} is not a folder inside the attachment. Use a relative path with no "..".`,
  NO_REPOSITORIES_CONFLICT: () => "You chose no repositories or drives, but something is attached. Remove it or remove the choice.",
  FIELD_UNAVAILABLE: (path: string) => `${path} is not available on this server yet, so the run was not created.`,
  BACKGROUND_ONLY: (door: string) =>
    `This run is a background task, so it has no ${door}. Its logs, audit trail and status stay available.`,
} as const;

/** Every sentence function by key, for the parity test and for callers that pick one by reason. */
export const RUN_MODE_REFUSAL_BY_KEY: Record<keyof typeof RUN_MODE_REFUSAL, (...args: string[]) => string> = {
  RUN_MODE_REQUIRED: () => RUN_MODE_REFUSAL.RUN_MODE_REQUIRED(),
  RUN_WORKLOAD_REQUIRED: () => RUN_MODE_REFUSAL.RUN_WORKLOAD_REQUIRED(),
  RUN_MODE_CONFLICT: (...a) => RUN_MODE_REFUSAL.RUN_MODE_CONFLICT(a[0], a[1]),
  STARTUP_TOOL_UNKNOWN: (...a) => RUN_MODE_REFUSAL.STARTUP_TOOL_UNKNOWN(a[0]),
  START_FOLDER_ATTACHMENT: (...a) => RUN_MODE_REFUSAL.START_FOLDER_ATTACHMENT(a[0]),
  START_FOLDER_SHAPE: (...a) => RUN_MODE_REFUSAL.START_FOLDER_SHAPE(a[0]),
  NO_REPOSITORIES_CONFLICT: () => RUN_MODE_REFUSAL.NO_REPOSITORIES_CONFLICT(),
  FIELD_UNAVAILABLE: (...a) => RUN_MODE_REFUSAL.FIELD_UNAVAILABLE(a[0]),
  BACKGROUND_ONLY: (...a) => RUN_MODE_REFUSAL.BACKGROUND_ONLY(a[0]),
};

/**
 * The server's subpath rule, for the console's own pre-check: relative, already
 * cleaned, no ".." element, no backslash. Whether the folder exists, and that no
 * symlink leaves the mount, is the server's, resolved inside the sandbox.
 */
export function startFolderSubpathOk(sub: string): boolean {
  if (sub === "") return true;
  if (sub.length > 512 || sub.startsWith("/") || sub.includes("\\")) return false;
  // The server refuses C0, DEL and C1 characters (controlCharFree).
  if (/[\u0000-\u001f\u007f-\u009f]/.test(sub)) return false;
  // Cleaned: no empty, "." or ".." element (a "//", a trailing "/", "./x").
  return !sub.split("/").some((part) => part === "" || part === "." || part === "..");
}
