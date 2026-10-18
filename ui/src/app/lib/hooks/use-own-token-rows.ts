/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The person's own-token Azure DevOps rows, one per organisation, in server order (#2146). /setup/status carries
// only the first row; GET /me/scm-access returns every one. Re-reads when `refresh` changes, so a page that
// reloads its status after a token is stored or removed shows the new state. A failed read is no rows: the
// page then shows what /setup/status already told it.
import * as React from "react";
import { scmAccess } from "../api/scm-access";
import type { SCMAccessPAT } from "../types/ado-pat";

export function useOwnTokenRows(refresh?: unknown): SCMAccessPAT[] {
  const [rows, setRows] = React.useState<SCMAccessPAT[]>([]);
  React.useEffect(() => {
    let alive = true;
    scmAccess
      .getMine()
      .then((all) => {
        if (alive) setRows((Array.isArray(all) ? (all as SCMAccessPAT[]) : []).filter((r) => r.token_mode === "own_pat"));
      })
      .catch(() => {
        if (alive) setRows([]);
      });
    return () => {
      alive = false;
    };
  }, [refresh]);
  return rows;
}
