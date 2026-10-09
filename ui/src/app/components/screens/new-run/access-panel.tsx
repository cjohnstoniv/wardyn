/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Access panel (#1922, #1914): one row per component the run carries, one
// open at a time, above the access facts New Run already states — what a saved
// policy grants on Azure DevOps and whether this person is connected. The Add
// control joins the rows in a later slice.
import type * as React from "react";
import type { RunPolicySpec } from "../../../lib/types";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { ADOAccessSummary } from "../../wardyn/ado-access-summary";
import { AdoLaunchNote, AdoRunTokenLine } from "../../wardyn/ado-run-token-line";
import { ACCESS_ROWS } from "../../wardyn/copy/components";
import { AccessRow } from "./access-row";
import type { AccessRow as Row } from "./access-rows-model";

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
  /** One per component the run carries (access-rows-model.ts). */
  rows: Row[];
  /** The one row that is open, by fact id. */
  openRowId: string | undefined;
  onOpenRow: (id: string | undefined) => void;
  /** Where a person adds a secret of their own. */
  secretsPath: string;
  guardLink: (to: string) => (e: React.MouseEvent) => void;
}

export function AccessPanel({
  adoAccess, savedMode, savedCaps, adoRefusal, adoConnecting, onAdoConnect,
  rows, openRowId, onOpenRow, secretsPath, guardLink,
}: AccessPanelProps) {
  return (
    <div className="space-y-2">
      {rows.length > 0 && (
        <ul aria-label={ACCESS_ROWS.LIST_LABEL} className="space-y-2">
          {rows.map((row) => (
            <AccessRow
              key={row.id}
              row={row}
              open={openRowId === row.id}
              onToggle={() => onOpenRow(openRowId === row.id ? undefined : row.id)}
              secretsPath={secretsPath}
              guardLink={guardLink}
            />
          ))}
        </ul>
      )}
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
