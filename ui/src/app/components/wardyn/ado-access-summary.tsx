/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one-line read-only summary of a policy's azure_devops_capabilities —
// "Azure DevOps: Read · Contribute" — shown under New Run's saved-policy picker
// and beside each row on the Policies screen, so a person picks a policy
// without opening it to find out what it grants.
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS } from "../../lib/ado-capabilities";
import { ADO_ACCESS, adoCapName } from "../../lib/ado-access-copy";
import { ADO_ENTRA_EDITOR, ADO_GROUP_COPY } from "../../lib/workspace-providers-copy";
import { Chip } from "./primitives";
import { cn } from "../ui/utils";

export function HighRiskBadge({ className }: { className?: string }) {
  return (
    <Chip tone="danger" className={cn("px-1.5 py-0 text-meta font-semibold", className)}>
      {ADO_ENTRA_EDITOR.HIGH_RISK_BADGE}
    </Chip>
  );
}

// adoAccessSummary names each group in catalogue order: a group chosen whole is
// its group name ("Contribute"); otherwise each chosen capability by name. A
// high-risk capability is always named on its own, never folded into a group.
// A non-list value (a hand-typed spec) reads as nothing chosen.
export function adoAccessSummary(caps: unknown): { parts: string[]; highRisk: boolean } {
  const chosen = new Set(Array.isArray(caps) ? caps : []);
  const parts: string[] = [];
  let highRisk = false;
  for (const g of ADO_CAPABILITY_GROUPS) {
    const inGroup = ADO_CAPABILITIES.filter((c) => c.group === g.id);
    const picked = inGroup.filter((c) => chosen.has(c.cap));
    if (picked.length === 0) continue;
    if (g.id === "high_risk") highRisk = true;
    if (g.id !== "high_risk" && picked.length === inGroup.length) parts.push(ADO_GROUP_COPY[g.id].name);
    else parts.push(...picked.map((c) => adoCapName(c.cap)));
  }
  return { parts, highRisk };
}

// Renders nothing for a policy that names no capabilities: it keeps the
// provider's default, which is the provider's to describe, not this line's.
export function ADOAccessSummary({ caps, className }: { caps: unknown; className?: string }) {
  const { parts, highRisk } = adoAccessSummary(caps);
  if (parts.length === 0) return null;
  return (
    <p className={cn("text-xs text-muted-foreground", className)} data-testid="ado-access-summary">
      {ADO_ACCESS.SUMMARY_PREFIX} <b className="font-semibold text-foreground">{parts.join(" · ")}</b>
      {highRisk && (
        <>
          {" "}
          <HighRiskBadge className="ml-1 align-middle" />
        </>
      )}
    </p>
  );
}
