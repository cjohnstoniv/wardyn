/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Access panel (#1922), first slice: the access facts New Run already
// states, moved here unchanged — what a saved policy grants on Azure DevOps and
// whether this person is connected. The per-component rows and the Add control
// replace this body in a later slice; the props are that seam.
import type * as React from "react";
import type { RunPolicySpec } from "../../../lib/types";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { ADOAccessSummary } from "../../wardyn/ado-access-summary";
import { AdoLaunchNote, AdoRunTokenLine } from "../../wardyn/ado-run-token-line";

export interface AccessPanelProps {
  /** /setup/status's Azure DevOps row for this caller, undefined when unknown. */
  adoAccess: SCMAccessPAT | undefined;
  /** The run launches a saved policy by reference; `savedCaps` is what the
   *  picked one grants on Azure DevOps (undefined until one is picked). */
  savedMode: boolean;
  savedCaps: RunPolicySpec["azure_devops_capabilities"];
  /** What the last refused launch said about the token, and the way to connect. */
  adoRefusal: React.ComponentProps<typeof AdoLaunchNote>["refusal"];
  adoConnecting: boolean;
  onAdoConnect: () => void;
}

export function AccessPanel({ adoAccess, savedMode, savedCaps, adoRefusal, adoConnecting, onAdoConnect }: AccessPanelProps) {
  return (
    <div className="space-y-2">
      {savedMode &&
        (adoAccess?.token_mode === "minted_pat" ? (
          <AdoRunTokenLine policyCaps={savedCaps} defaults={adoAccess.default_profile} />
        ) : (
          <ADOAccessSummary caps={savedCaps} />
        ))}
      {/* A launch on a row that creates a token per run is refused until the
          person has connected (or while the organisation blocks it). */}
      <AdoLaunchNote access={adoAccess} refusal={adoRefusal} connecting={adoConnecting} onConnect={onAdoConnect} />
    </div>
  );
}
