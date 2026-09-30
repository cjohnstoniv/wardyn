/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The console's Azure DevOps capability catalogue: every grantable capability
// (adoscope.GrantableCapabilities), the Azure DevOps area it is shown under,
// and whether it is high risk. One module so the policy editor and the
// provider-row editor cannot group or flag a capability differently. Names,
// consequence lines and the grey Azure DevOps line live in
// workspace-providers-copy.ts (ADO_CAP_COPY); a capability without one renders
// as its wire name.

export type ADOCapabilityGroup = "repos" | "boards" | "wiki" | "pipelines" | "artifacts" | "test_plans" | "organization";

// Azure DevOps' own services, in the approved mock's order. Titles live in
// ADO_GROUP_COPY.
export const ADO_CAPABILITY_GROUPS: readonly { id: ADOCapabilityGroup }[] = [
  { id: "repos" },
  { id: "boards" },
  { id: "wiki" },
  { id: "pipelines" },
  { id: "artifacts" },
  { id: "test_plans" },
  { id: "organization" },
];

export interface ADOCapabilityInfo {
  cap: string;
  group: ADOCapabilityGroup;
  highRisk: boolean;
  // One of the twelve per-area reads the summary folds into "Read (every area)".
  read: boolean;
}

// In the approved mock's order: area by area, and within an area read → write
// → admin, the way Azure DevOps' own scopes go. High-risk rows sit inside their
// area. ado-capabilities.test.ts pins the set to internal/adoscope/capability.go
// and the order to this list.
export const ADO_CAPABILITIES: readonly ADOCapabilityInfo[] = [
  { cap: "code_read", group: "repos", highRisk: false, read: true },
  { cap: "code_write", group: "repos", highRisk: false, read: false },
  { cap: "pr", group: "repos", highRisk: false, read: false },
  { cap: "policy_admin", group: "repos", highRisk: true, read: false },
  { cap: "policy_bypass", group: "repos", highRisk: true, read: false },
  { cap: "repo_admin", group: "repos", highRisk: true, read: false },
  { cap: "work_read", group: "boards", highRisk: false, read: true },
  { cap: "work_write", group: "boards", highRisk: false, read: false },
  { cap: "work_admin", group: "boards", highRisk: true, read: false },
  { cap: "wiki_read", group: "wiki", highRisk: false, read: true },
  { cap: "wiki_write", group: "wiki", highRisk: false, read: false },
  { cap: "build_read", group: "pipelines", highRisk: false, read: true },
  { cap: "build_execute", group: "pipelines", highRisk: false, read: false },
  { cap: "build_admin", group: "pipelines", highRisk: true, read: false },
  { cap: "release_read", group: "pipelines", highRisk: false, read: true },
  { cap: "release_execute", group: "pipelines", highRisk: false, read: false },
  { cap: "release_admin", group: "pipelines", highRisk: true, read: false },
  { cap: "serviceendpoint_read", group: "pipelines", highRisk: false, read: true },
  { cap: "serviceendpoint_admin", group: "pipelines", highRisk: true, read: false },
  { cap: "library_read", group: "pipelines", highRisk: false, read: true },
  { cap: "packaging_read", group: "artifacts", highRisk: false, read: true },
  { cap: "packaging_write", group: "artifacts", highRisk: false, read: false },
  { cap: "packaging_manage", group: "artifacts", highRisk: true, read: false },
  { cap: "test_read", group: "test_plans", highRisk: false, read: true },
  { cap: "project_read", group: "organization", highRisk: false, read: true },
  { cap: "identity_read", group: "organization", highRisk: false, read: true },
  { cap: "analytics_read", group: "organization", highRisk: false, read: true },
  { cap: "project_admin", group: "organization", highRisk: true, read: false },
  { cap: "security_admin", group: "organization", highRisk: true, read: false },
];

// What an empty default_profile means on the server (adoscope.ProfileDefault).
export const ADO_DEFAULT_PROFILE: readonly string[] = ["project_read", "code_read"];
