/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The MEMBER-facing strings of the approved Azure DevOps access mock (the
// policy editor section and the saved-policy summary line). Group and
// capability names are shared with the admin editor and live in
// workspace-providers-copy.ts (ADO_GROUP_COPY, ADO_CAP_COPY, ADO_ENTRA_EDITOR).
import { ADO_CAP_COPY, ADO_ENTRA_EDITOR, ADO_GROUP_COPY } from "./workspace-providers-copy";
import type { ADOCapabilityGroup } from "./ado-capabilities";

export const ADO_ACCESS = {
  SECTION_TITLE: "Azure DevOps access",
  SECTION_LEAD:
    "What a run launched with this policy may do against Azure DevOps — bound by your administrator's ceiling on the row it runs against.",
  HIGH_RISK_WARN_MEMBER: "Grant only what this run actually needs — these reach past this one run.",
  LOCKED: "Not allowed by your administrator.",
  SUMMARY_PREFIX: "Azure DevOps:",
} as const;

// The member editor heads the high-risk group with the short "High risk", as
// the mock does; the admin ceiling uses the group's longer name.
export function adoGroupName(id: ADOCapabilityGroup): string {
  return id === "high_risk" ? ADO_ENTRA_EDITOR.HIGH_RISK_BADGE : (ADO_GROUP_COPY[id]?.name ?? id);
}

export function adoCapName(cap: string): string {
  return ADO_CAP_COPY[cap]?.name ?? cap;
}
