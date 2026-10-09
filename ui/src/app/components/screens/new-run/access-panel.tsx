/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Access panel (#1922, #1914): one row per component the run carries, one
// open at a time, above the access facts New Run already states — what a saved
// policy grants on Azure DevOps and whether this person is connected. The Add
// control lists what else the person may add; its dialogs load on demand.
import * as React from "react";
import { Plus } from "lucide-react";
import type { ComponentRef, RunPolicySpec } from "../../../lib/types";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { ADOAccessSummary } from "../../wardyn/ado-access-summary";
import { AdoLaunchNote, AdoRunTokenLine } from "../../wardyn/ado-run-token-line";
import { Button } from "../../ui/button";
import { ACCESS_ROWS, ADD_ACCESS } from "../../wardyn/copy/components";
import { AccessRow } from "./access-row";
import type { AccessRow as Row } from "./access-rows-model";
import { refIndexForFact } from "./custom-component-form-model";

const AddAccessDialog = React.lazy(() => import("./add-access-dialog").then((m) => ({ default: m.AddAccessDialog })));

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
  /** What the run carries (CreateRunRequest.components) and how it changes. With
   *  no way to change it there is no Add control. */
  refs?: ComponentRef[];
  onRefsChange?: (next: ComponentRef[]) => void;
}

export function AccessPanel({
  adoAccess, savedMode, savedCaps, adoRefusal, adoConnecting, onAdoConnect,
  rows, openRowId, onOpenRow, secretsPath, guardLink, refs = [], onRefsChange,
}: AccessPanelProps) {
  const [adding, setAdding] = React.useState(false);
  // Only a component the person added can be taken off; a row the run carries for another reason has no remove.
  const removeOf = (rowId: string) => {
    const at = refIndexForFact(rowId, refs);
    return onRefsChange && at >= 0 ? () => onRefsChange(refs.filter((_, i) => i !== at)) : undefined;
  };
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
              onRemove={removeOf(row.id)}
            />
          ))}
        </ul>
      )}
      {onRefsChange && (
        <div>
          <Button type="button" variant="outline" size="sm" onClick={() => setAdding(true)}>
            <Plus className="size-3.5" />
            {ADD_ACCESS.BUTTON}
          </Button>
          {adding && (
            <React.Suspense fallback={null}>
              <AddAccessDialog
                refs={refs}
                secretsPath={secretsPath}
                guardLink={guardLink}
                onAdd={(ref) => onRefsChange([...refs, ref])}
                onClose={() => setAdding(false)}
              />
            </React.Suspense>
          )}
        </div>
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
