/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Canon copy for the member-facing "Azure DevOps access" surfaces — the policy
// editor section and the saved-policy summary line —
// verbatim from the owner-approved mock (mock-08, ado-entra-editor-g1 packet).
// Keyed by the ids in ado-capabilities.ts. The capability names are also
// authored in Go (adoscope.ShortLabel) for the dispatch refusal;
// ado-access-copy.test.ts pins the two together.
import type { ADOCapabilityGroup } from "./ado-capabilities";

export const ADO_ACCESS = {
  SECTION_TITLE: "Azure DevOps access",
  SECTION_LEAD:
    "What a run launched with this policy may do against Azure DevOps — bound by your administrator’s ceiling on the row it runs against.",
  HIGH_RISK_BADGE: "High risk",
  HIGH_RISK_WARN_MEMBER: "Grant only what this run actually needs — these reach past this one run.",
  LOCKED: "Not allowed by your administrator.",
  SUMMARY_PREFIX: "Azure DevOps:",
} as const;

// The member editor's group headings (the admin screen's high-risk heading is
// the longer "High risk — admin-level changes").
export const ADO_GROUP_NAME: Record<ADOCapabilityGroup, string> = {
  read: "Read",
  contribute: "Contribute",
  work: "Work tracking",
  pipelines: "Pipelines & packages",
  high_risk: "High risk",
};

export const ADO_CAP_NAME: Record<string, string> = {
  read: "Read",
  code_write: "Push to the run’s own branch",
  pr: "Open pull requests",
  work_write: "Work items",
  wiki_write: "Wiki",
  build_execute: "Run pipelines & releases",
  packaging_write: "Publish packages",
  policy_admin: "Change branch policies",
  policy_bypass: "Bypass branch policies",
  repo_admin: "Manage repositories",
  security_admin: "Change permissions & identities",
  serviceendpoint_admin: "Manage service connections",
  build_admin: "Change pipeline definitions",
  project_admin: "Manage projects",
};
