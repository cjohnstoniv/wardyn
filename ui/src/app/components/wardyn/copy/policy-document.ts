/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shared policy document and source editor's copy. Canon is the approved
// M-R prototype (version 7), character for character; policy-document-copy.test.ts
// compares every string here with that list. Imported directly by the lazy
// policy surfaces, never re-exported through the eager copy.ts barrel.
import type { PolicyPreviewPending } from "../../../lib/types/policy-preview";

export const POLICY_DOCUMENT = {
  // Read and edit modes.
  THIS_RUN: "This run's policy",
  READ_EDIT: "Read-only. Choose Edit policy to change it.",
  READ_CUSTOMIZE: "Read-only. Choose Customize for this run to change it.",
  READ_UPDATES: "Read-only. It updates as you edit the policy below.",
  MERGED: "It also includes what this run's selections add and what your limits change.",
  EDIT: "Edit policy",
  DONE: "Done editing",
  EDITING: "Editing",
  CUSTOMIZE: "Customize for this run",
  SOURCE_POLICY: "Source policy",
  // Views and copies. Summary, YAML and Copy YAML stay in policy-tab-copy.ts.
  JSON: "JSON",
  COPY_JSON: "Copy JSON",
  COPY_SOURCE: "Copy source",
  // The source editor.
  SPEC_YAML: "Spec (YAML)",
  SPEC_JSON: "Spec (JSON)",
  VALID_YAML: "Valid YAML",
  INVALID_YAML: (message: string) => `Invalid YAML — ${message}`,
  VALID_JSON: "Valid JSON",
  INVALID_JSON: (message: string) => `Invalid JSON — ${message}`,
  SOURCE_POSITION: (line: number, column: number) => `Line ${line}, column ${column}`,
  INVALID_GATE: "The policy spec isn't valid YAML or JSON.",
  COMMENTS: "Comments are kept while you edit; they are not stored when the policy is saved or the run launches.",
  SAFE_CUSTOM: "Some source settings are hidden. Customization starts from a safe policy.",
  // Preview states.
  INVALID_PREVIEW: "This preview is out of date. Fix the policy source to refresh it.",
  STALE_PREVIEW: "This preview is out of date. Check again to refresh it.",
  PROVISIONAL: "This preview includes your selections. Launch checks may change it.",
  PENDING: "Not checked in this preview.",
  RATE_LIMIT: (seconds: number) => `Preview limit reached. Try again in ${seconds}s.`,
  // Confirmations.
  JSON_TITLE: "Switch to JSON?",
  JSON_BODY: "JSON does not preserve YAML comments. Switching changes your editable source.",
  KEEP_YAML: "Keep YAML",
  SWITCH_JSON: "Switch to JSON",
  REPLACE_TITLE: "Replace your custom policy?",
  REPLACE_BODY: "Your existing custom policy will be replaced by this source.",
  KEEP_CUSTOM: "Keep custom policy",
  REPLACE_CUSTOM: "Replace custom policy",
  // Summary rows the run page's canon (policy-tab-copy.ts SUMMARY) does not carry.
  REQUESTED_CLASS: "This run requests",
  ALLOW_ALL: "Allow-all egress (block-list only)",
  HOLDS_AT_ONCE: "Holds at once",
  OTHER_SETTINGS: "Other settings",
  STOPS_AFTER: (minutes: number) => `Stops after ${minutes} minutes`,
  TOOL_RULE: (tool: string, effect: string) => `${tool} — ${effect}`,
  CHECKED_AT_LAUNCH: "Checked at launch",
} as const;

// What each launch-only check is called in the "Checked at launch" list.
export const PENDING_NAME: Record<PolicyPreviewPending, string> = {
  task: "Task",
  model_provider_selection: "Model provider",
  credential_liveness: "Credentials still valid",
  autonomy: "Autonomy",
  tool_approvals: "Tool approvals",
  runner_confinement: "Barrier on the runner",
  drive_readiness: "Drive ready",
  dispatch_egress: "Hosts added at dispatch",
};
