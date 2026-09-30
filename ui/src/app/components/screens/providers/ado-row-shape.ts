/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shape an Azure DevOps row must have to be saved (#1429, #1428). The
// shared token and key lanes are retired, so an empty `lanes` is refused, and
// which lane is valid follows the row's addresses: a Services row (dev.azure.com
// or *.visualstudio.com) signs people in through the entra lane, with a token
// mode; a Server row has no sign-in, so people add their own token through the
// pat lane, per person, with no entra block. The console writes the valid one
// as the addresses are typed, so "Add provider" never authors a row the server
// refuses.
import type { ADOEntraConfig, GitProvider } from "../../../lib/api/providers";
import { ADO_DEFAULT_PROFILE } from "../../../lib/ado-capabilities";
import { adoIsServer } from "../../../lib/ado-pat-display";
import { baseURLError } from "./display";

/** A fresh Services row's Entra block: a token for each run, reads only. The
 *  tenant and client are the admin's to fill; the default profile reads as the
 *  ceiling's reads when empty. */
function newEntra(): ADOEntraConfig {
  return {
    tenant_id: "",
    client_id: "",
    token_mode: "minted_pat",
    capability_ceiling: [...ADO_DEFAULT_PROFILE],
    default_profile: [],
  };
}

/** The row "Add provider" makes for Azure DevOps. Its address has no
 *  organisation on purpose (the server refuses it, and the row says so) until
 *  the admin appends theirs. */
export function newADORow(id: string): GitProvider {
  return {
    id,
    kind: "azure_devops",
    base_urls: ["https://dev.azure.com/"],
    lanes: ["entra"],
    credential_source: "per_user",
    entra: newEntra(),
  };
}

/** Whether a row is an Azure DevOps Server row: some address is not a Services
 *  host. */
export function isADOServerRow(row: GitProvider): boolean {
  return row.kind === "azure_devops" && row.base_urls.some(adoIsServer);
}

/** The row with the lane its addresses call for. `stashed` is the Entra block the
 *  row lost on its way to Server, restored when the addresses are Services again
 *  (a mistyped but valid host reshapes the row, and correcting it must not cost
 *  the tenant, client and ceiling already entered). Only complete, valid addresses
 *  reshape it: a half-typed one is left alone, so typing "https://dev.azure.com/"
 *  letter by letter never flips the row to Server and back, discarding what was
 *  entered on the way. */
export function reshapeADORow(row: GitProvider, stashed?: ADOEntraConfig): GitProvider {
  if (row.kind !== "azure_devops" || row.base_urls.length === 0) return row;
  if (row.base_urls.some((u) => baseURLError(u, row.kind) !== null)) return row;
  const hasEntra = !!row.entra && !!row.lanes?.includes("entra");
  if (isADOServerRow(row)) {
    if (!hasEntra && row.lanes?.length === 1 && row.lanes[0] === "pat" && row.credential_source === "per_user") return row;
    const { entra: _dropped, ...rest } = row;
    void _dropped;
    return { ...rest, lanes: ["pat"], credential_source: "per_user" };
  }
  if (hasEntra) return row;
  // Back to Services: what the admin entered before the row went to Server comes back.
  return { ...row, lanes: ["entra"], credential_source: "per_user", entra: stashed ?? newEntra() };
}
