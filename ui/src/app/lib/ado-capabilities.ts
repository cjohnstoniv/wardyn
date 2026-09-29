/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The console's Azure DevOps capability catalogue: every grantable capability
// (adoscope.GrantableCapabilities), the group it is shown under, and whether it
// is high risk. One module so the policy editor and the provider-row editor
// cannot group or flag a capability differently. Labels and hints stay in
// ado-entra-copy.ts; a capability without one renders as its wire name.
//
// DRAFT: group names await the approved mock (mock-first law).
import { ADO } from "./ado-entra-copy";

export type ADOCapabilityGroup = "read" | "contribute" | "work" | "pipelines" | "high_risk";

export const ADO_CAPABILITY_GROUPS: readonly { id: ADOCapabilityGroup; title: string }[] = [
  { id: "read", title: "Read" },
  { id: "contribute", title: "Contribute" },
  { id: "work", title: "Work tracking" },
  { id: "pipelines", title: "Pipelines & packages" },
  { id: "high_risk", title: "High-risk" },
];

export interface ADOCapabilityInfo {
  cap: string;
  group: ADOCapabilityGroup;
  highRisk: boolean;
  label?: string;
  hint?: string;
}

// In the approved mock's order: group by group, and within a group the order
// the mock draws (High risk leads with policy_admin). ado-capabilities.test.ts
// pins the set to internal/adoscope/capability.go and the order to this list.
export const ADO_CAPABILITIES: readonly ADOCapabilityInfo[] = [
  { cap: "read", group: "read", highRisk: false, label: ADO.CAP_READ, hint: ADO.CAP_READ_HINT },
  { cap: "code_write", group: "contribute", highRisk: false, label: ADO.CAP_CODE_WRITE, hint: ADO.CAP_CODE_WRITE_HINT },
  { cap: "pr", group: "contribute", highRisk: false, label: ADO.CAP_PR, hint: ADO.CAP_PR_HINT },
  { cap: "work_write", group: "work", highRisk: false, label: ADO.CAP_WORK_WRITE, hint: ADO.CAP_WORK_WRITE_HINT },
  { cap: "wiki_write", group: "work", highRisk: false, label: ADO.CAP_WIKI_WRITE, hint: ADO.CAP_WIKI_WRITE_HINT },
  { cap: "build_execute", group: "pipelines", highRisk: false, label: ADO.CAP_BUILD_EXECUTE, hint: ADO.CAP_BUILD_EXECUTE_HINT },
  { cap: "packaging_write", group: "pipelines", highRisk: false },
  { cap: "policy_admin", group: "high_risk", highRisk: true, label: ADO.CAP_POLICY_ADMIN, hint: ADO.CAP_POLICY_ADMIN_HINT },
  { cap: "policy_bypass", group: "high_risk", highRisk: true, label: ADO.CAP_POLICY_BYPASS, hint: ADO.CAP_POLICY_BYPASS_HINT },
  { cap: "repo_admin", group: "high_risk", highRisk: true, label: ADO.CAP_REPO_ADMIN, hint: ADO.CAP_REPO_ADMIN_HINT },
  { cap: "security_admin", group: "high_risk", highRisk: true },
  { cap: "serviceendpoint_admin", group: "high_risk", highRisk: true },
  { cap: "build_admin", group: "high_risk", highRisk: true },
  { cap: "project_admin", group: "high_risk", highRisk: true },
];
