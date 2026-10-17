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
  /** shape does not fit the run's experience, workload or tools. */
  SHAPE_CONFLICT: "run_shape_conflict",
  /** The harness set: each body names the harness in `tool` (HttpError.tool). */
  HARNESS_NOT_INSTALLED: "harness_not_installed",
  HARNESS_SET_NO_IMAGE: "harness_set_no_image",
  HARNESS_SET_CUSTOM_IMAGE: "harness_set_custom_image",
  HARNESS_PROVIDERS_CONFLICT: "harness_providers_conflict",
  /** The run's policy writes a disk size that the runner it lands on cannot cap. */
  MAIN_DRIVE_UNENFORCEABLE: "main_drive_unenforceable",
} as const;

export type RunModeReason = (typeof RUN_MODE_REASON)[keyof typeof RUN_MODE_REASON];

/**
 * The policy_overlay refusals (internal/api/run_policy_overlay.go). Kept apart from
 * RUN_MODE_REASON: no screen sends policy_overlay yet, so none has a home for them.
 */
export const POLICY_OVERLAY_REASON = {
  /** policy_overlay is not a non-empty object of policy keys with usable values. */
  INVALID: "policy_overlay_invalid",
  /** policy_overlay rides beside policy_id or inline_policy. */
  CONFLICT: "policy_overlay_conflict",
} as const;

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
  IDLE_STOP_BACKGROUND: () => "A background task ends when its work exits, so it has no idle stop. Remove it, or choose an interactive environment.",
  RUN_SHAPE_CONFLICT: (shape: string, detail: string) => `shape ${shape} does not fit this run: ${detail}.`,
  STARTUP_PROMPT_SHAPE: () => "startup.prompt: only a harness startup takes a starting prompt",
  HARNESS_NOT_INSTALLED: (name: string) =>
    `${name} isn't installed in this server's sandbox images, so the run was not created. Ask an admin to add an image that includes it.`,
  HARNESS_SET_NO_IMAGE: (...names: string[]) => {
    const list = names.length > 2 ? `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}` : names.join(" and ");
    const both = names.length > 2 ? "all of them" : "both";
    return `No sandbox image on this server includes ${list} together, so the run was not created. Remove one, or ask an admin to add an image with ${both}.`;
  },
  HARNESS_SET_CUSTOM_IMAGE: () =>
    "A run with more than one harness starts from an image your admin declared for them, so it can't use a custom image.",
  HARNESS_PROVIDERS_CONFLICT: (a: string, b: string) =>
    `${a} and ${b} use different model providers that reach the same host, so the proxy can't keep their credentials apart. Choose one provider for both, or another provider.`,
  HARNESS_SIGNIN_NOT_PRIMARY: (harness: string, provider: string) =>
    `Model provider ${provider} uses an AWS sign-in, which only the harness a run starts, or else lists first, can use, so the run was not created. Make ${harness} that harness, or choose another provider for it.`,
  MAIN_DRIVE_UNENFORCEABLE: () => "This run's policy sets a disk size that this pool's runners can't cap. Choose another pool, or ask your admin.",
  CODEX_BUILTIN_RULE: (tool: string) =>
    `${tool} is one of Codex CLI's own tools, and Wardyn can't block those. Only its MCP tools can be blocked.`,
  POLICY_OVERLAY_INVALID: (path: string, detail: string) => `policy_overlay is not usable: ${path} ${detail}.`,
  POLICY_OVERLAY_CONFLICT: () =>
    "policy_overlay changes your organisation's default policy, so it cannot be combined with policy_id or inline_policy.",
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
  IDLE_STOP_BACKGROUND: () => RUN_MODE_REFUSAL.IDLE_STOP_BACKGROUND(),
  RUN_SHAPE_CONFLICT: (...a) => RUN_MODE_REFUSAL.RUN_SHAPE_CONFLICT(a[0], a[1]),
  STARTUP_PROMPT_SHAPE: () => RUN_MODE_REFUSAL.STARTUP_PROMPT_SHAPE(),
  HARNESS_NOT_INSTALLED: (...a) => RUN_MODE_REFUSAL.HARNESS_NOT_INSTALLED(a[0]),
  HARNESS_SET_NO_IMAGE: (...a) => RUN_MODE_REFUSAL.HARNESS_SET_NO_IMAGE(...a),
  HARNESS_SET_CUSTOM_IMAGE: () => RUN_MODE_REFUSAL.HARNESS_SET_CUSTOM_IMAGE(),
  HARNESS_PROVIDERS_CONFLICT: (...a) => RUN_MODE_REFUSAL.HARNESS_PROVIDERS_CONFLICT(a[0], a[1]),
  HARNESS_SIGNIN_NOT_PRIMARY: (...a) => RUN_MODE_REFUSAL.HARNESS_SIGNIN_NOT_PRIMARY(a[0], a[1]),
  MAIN_DRIVE_UNENFORCEABLE: () => RUN_MODE_REFUSAL.MAIN_DRIVE_UNENFORCEABLE(),
  CODEX_BUILTIN_RULE: (...a) => RUN_MODE_REFUSAL.CODEX_BUILTIN_RULE(a[0]),
  POLICY_OVERLAY_INVALID: (...a) => RUN_MODE_REFUSAL.POLICY_OVERLAY_INVALID(a[0], a[1]),
  POLICY_OVERLAY_CONFLICT: () => RUN_MODE_REFUSAL.POLICY_OVERLAY_CONFLICT(),
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
