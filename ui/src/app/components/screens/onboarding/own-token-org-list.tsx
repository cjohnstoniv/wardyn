/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Getting started's Azure DevOps line when the person has own-token rows for more than one organisation
// (#2146): one line per organisation, each with its own button, in place of the single first-row line.
// One row never reaches this list; its page is unchanged.
import * as React from "react";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { ownTokenOrgLine } from "../../../lib/scm-access-display";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { AdoOwnTokenDialog } from "../settings/ado-own-token-dialog";

export function OwnTokenOrgList({ rows, onStored }: { rows: readonly SCMAccessPAT[]; onStored: () => void }) {
  const [editing, setEditing] = React.useState<SCMAccessPAT | null>(null);
  return (
    <div className="mt-2 space-y-1.5" data-testid="ado-own-token-orgs">
      <p className="text-sm font-medium">{PROVIDERS.KIND_AZURE_DEVOPS}</p>
      <ul className="space-y-1.5">
        {rows.map((row) => {
          const line = ownTokenOrgLine(row);
          return (
            <li key={row.org} className="flex flex-wrap items-center gap-2 text-sm" data-testid={`ado-own-org-${line.org}`}>
              <span className="font-mono">{line.org}</span>
              <Chip tone={line.tone}>{line.label}</Chip>
              {line.button && (
                <Button size="sm" variant="outline" onClick={() => setEditing(row)}>
                  {line.button === "add" ? ADO_PAT.OWN_ADD_CTA : ADO_PAT.OWN_REPLACE}
                </Button>
              )}
            </li>
          );
        })}
      </ul>
      {editing && (
        <AdoOwnTokenDialog
          open
          onOpenChange={(o) => !o && setEditing(null)}
          address={editing.org ?? ""}
          days={editing.max_days ?? 30}
          scopes={editing.token_scopes}
          onStored={onStored}
        />
      )}
    </div>
  );
}
