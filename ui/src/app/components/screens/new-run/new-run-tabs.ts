/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run tabs (owner, 2026-10-09: "New Run tabs restructured") and which
// tab owns each form field: Info → Workspaces → Runner → Access → Policy. The
// map is typed over every WizardState key, so a field added to the form fails
// the build until it is given a tab. The panels (A-L6…A-L10) and the controller
// (A-L8) read their fields from here; `NewRunPanelId` in new-run-launch-gates.ts
// still names the four old panels until A-L8 renames them.
import type { WizardState } from "./wizard-types";
import type { RunContractDraft } from "../../../lib/run-contract-draft";

export const NEW_RUN_TABS = ["info", "workspaces", "runner", "access", "policy"] as const;
export type NewRunTab = (typeof NEW_RUN_TABS)[number];

/**
 * - Info: run metadata only.
 * - Workspaces: the attached workspaces and the drive attach (target collisions live here).
 * - Runner: Runs on, agent, run type, mode and what it does at boot, the image,
 *   Barrier, CPU/Memory (the `contract.runner` draft) and lifetime (auto-stop).
 * - Access: the model provider pick, tool approvals, components and the per-component
 *   overrides (`contract.access`).
 * - Policy: egress, the policy choice and save-as-profile.
 */
export const TAB_OF_FIELD: Record<Exclude<keyof WizardState, "contract">, NewRunTab> = {
  title: "info",
  description: "info",

  workspaces: "workspaces",
  driveEnabled: "workspaces",
  driveReadOnly: "workspaces",

  runType: "runner",
  agent: "runner",
  mode: "runner",
  task: "runner",
  command: "runner",
  startupCommand: "runner",
  interactiveStart: "runner",
  seedAutoTools: "runner",
  image: "runner",
  confinementClass: "runner",
  lifecycle: "runner",
  autoStopMinutes: "runner",

  toolApprovals: "access",
  modelProviderId: "access",
  integrationId: "access",
  components: "access",
  githubEnabled: "access",
  githubRepos: "access",
  githubPermission: "access",
  githubRequiresApproval: "access",
  githubTtlMinutes: "access",
  llmSecretName: "access",
  gitPatEnabled: "access",
  gitPatHost: "access",
  gitPatSecretName: "access",
  gitPatUsername: "access",
  gitPatRequiresApproval: "access",

  selectedPolicyId: "policy",
  allowedDomains: "policy",
  deniedDomains: "policy",
  firstUseApproval: "policy",
  allowAllEgress: "policy",
  saveAsProfile: "policy",
  profileName: "policy",
};

/** The contract draft spans two tabs; each group belongs to its own tab. */
export const TAB_OF_CONTRACT: Record<keyof RunContractDraft, NewRunTab> = {
  runner: "runner",
  access: "access",
};

/** The WizardState keys one tab owns. */
export function fieldsOfTab(tab: NewRunTab): (keyof typeof TAB_OF_FIELD)[] {
  return (Object.keys(TAB_OF_FIELD) as (keyof typeof TAB_OF_FIELD)[]).filter((k) => TAB_OF_FIELD[k] === tab);
}
