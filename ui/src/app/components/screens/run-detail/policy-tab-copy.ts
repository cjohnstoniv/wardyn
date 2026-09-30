/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Policy tab copy. Canon is the owner-approved mock
// (run-policy-view-packet.html, strings S-1..S-48), character for character:
// policy-tab-copy.test.ts pins every string, so a rewording fails a test
// instead of drifting. S-36, S-39, S-42 (labels), S-43, S-44 (idle value) and
// S-46 are REUSED from the console's existing sources and are not repeated here.
import type { RunPolicyCause } from "../../../lib/types";

export const POLICY_TAB = {
  // S-1
  tab: "Policy",
  // S-2
  lead: "What this run was allowed to do, including anything Wardyn changed when it started.",
  // S-3 .. S-9: the source line.
  sourceStored: (name: string) => `Started from the saved policy "${name}".`,
  sourceStoredDeleted: (name: string) => `Started from the saved policy "${name}", which has since been deleted.`,
  sourceStoredDeletedNoName: "Started from a saved policy that has since been deleted.",
  sourceInline: "Started from a policy written for this run.",
  sourceDefault: "Started from your organization's default policy.",
  sourceProfile: (name: string) => `Started from the ${name} governance profile.`,
  sourceUnknown: "Wardyn set this policy for this run.",
  // S-10
  preset: (name: string, version: number) => `Launched from the preset "${name}", version ${version}.`,
  // S-11a / S-11b
  changedSince: (name: string) =>
    `The saved policy "${name}" has changed since this run started. This page shows what the run got.`,
  updatedSince: (name: string) =>
    `The saved policy "${name}" was updated after this run started, so it may read differently now. This page shows what the run got.`,
  // S-12 / S-13
  changesHeading: "Changed when the run started",
  nothingChanged: "Nothing was changed. The run got the policy exactly as written.",
  // S-24
  chipAdded: "Added at start",
  chipRemoved: "Removed at start",
  // S-25 / S-26
  viewSummary: "Summary",
  viewYaml: "YAML",
  copyYaml: "Copy YAML",
  // S-27 / S-28 / S-29 / S-30
  notYet: "Wardyn records this run's policy when its sandbox is set up. This run hasn't reached that step.",
  never: "This run stopped before its sandbox was set up, so no policy was applied to it.",
  loadError: "Couldn't load this run's policy.",
  incomplete: "This run started before Wardyn recorded each change, so some changes may not be listed.",
  // S-31
  hidden: "Hidden",
  hiddenTip: "Only admins can see this.",
  // S-32
  scope:
    "Not shown here: hosts approved while the run was running (see Approvals), credentials it was handed (see Credentials on Overview), folders added from a workspace or drive, and Azure DevOps access that came from the connection's defaults.",
  // S-33
  redacted: "Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.",
} as const;

// S-14a / S-14b .. S-23: the change-group headings, one per cause. The three
// that need a name or a date (S-14b, S-20, S-21) take it from the change.
export const CHANGE_HEADING = {
  limitsOwn: "Narrowed to fit your limits", // S-14a
  limitsOther: (person: string) => `Narrowed to fit the limits set for ${person}`, // S-14b
  workspace: "Added for the workspace", // S-15
  source_control: "Added so the run can reach its code", // S-16
  git_broker: "Routed through Wardyn's GitHub connection", // S-17
  model_access: "Added so the agent can reach its model", // S-18
  mirror: "Switched to your organization's package mirror", // S-19
  profile: (name: string) => `Limited by the ${name} governance profile`, // S-20
  restart: (date: string) => `Blocked when the run was restarted on ${date}`, // S-21
  org_disk: "Disk size set from your organization's default", // S-22
  launch: "Set by Wardyn when the run started", // S-23
} as const;

export type PlainCause = Exclude<RunPolicyCause, "limits" | "profile" | "restart">;

// S-34 .. S-47: the Summary view.
export const SUMMARY = {
  network: "Network",
  barrier: "Barrier",
  credentials: "Credentials",
  files: "Files and code",
  tools: "Tools and pushes",
  apps: "Apps",
  limits: "Limits",
  traffic: "Traffic checks",
  ado: "Azure DevOps access",
  // S-35
  allowedHosts: "Allowed hosts",
  blockedHosts: "Blocked hosts",
  otherHost: "Any other host",
  requestTypes: "Request types",
  // S-37
  refused: "Refused",
  refusedThenApproval: "Refused, then sent for approval",
  held: (n: number) => `Held for up to ${n} seconds while someone decides`,
  // S-38
  allMethods: "All",
  // S-39
  minimum: "Minimum",
  used: "This run used",
  // S-40
  grantKinds: {
    github_token: "GitHub access",
    api_key: "API key",
    git_pat: "Git access token",
    ssh_key: "SSH key",
    cloud_sts: "Cloud credentials",
    env_secret: "Environment secret",
  } as Record<string, string>,
  needsApproval: "Needs approval",
  // S-41
  folders: "Folders from the host",
  repos: "Repositories",
  readOnly: "Read-only",
  // S-42
  toolRules: "Tool rules",
  pushes: "Pushes",
  // Reused from the policy editor's push-rule sections (policy-push-rules.tsx).
  pushDeny: "Deny",
  pushHold: "Hold for review",
  anyBranch: "Any branch",
  ownBranch: "Only this run's own branch",
  // S-44
  cpu: "CPU",
  memory: "Memory",
  processes: "Processes",
  disk: "Disk",
  idle: "When idle",
  standardLimit: "Standard limit",
  cpuValue: (n: string) => `${n} CPU`,
  mibValue: (n: number) => `${n} MiB`,
  // S-45
  off: "Off",
  on: "On",
  // S-47
  none: "None",
} as const;

// S-48
export const IDENTITY_POLICY_VIEW = "View";
