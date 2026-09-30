/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The per-person Azure DevOps token routes (#1428, #1430). The organisation
// check and the own-token doors are the server lanes' (internal/api
// ado_pat_orgcheck.go, ado_own_pat.go). Disconnect and a run's token list have
// no server route yet: their paths and shapes are this console's assumption,
// spliced at the route in the e2e specs, and a daemon without them fails like
// any missing route, leaving the card that asked as it was.
import type { ADOOrgCheck, ADOOwnTokenBody, ADORunToken } from "../types/ado-pat";
import { asJson, errText, HttpError, wfetch } from "./core";

// The reasons an own-token store answers with (internal/api reasons_routes.go,
// docs/sdk.md). The first three are drawn under their field in the mock's own
// words; any other is the server's sentence, verbatim.
export const OWN_TOKEN_REASON = {
  MISMATCH: "ado_own_pat_identity_mismatch",
  TOO_LONG: "ado_own_pat_expiry_too_long",
  REJECTED: "ado_own_pat_rejected",
} as const;

// The reasons a personal-access-token creation is refused with (reasons.go,
// docs/sdk.md), as the envelope's `reason`. ado-pat-display.ts's patRefusalNote
// maps each to console copy; the server's own sentence still renders verbatim
// in the launch strip.
export const ADO_PAT_REASON = {
  POLICY_BLOCKED: "ado_pat_policy_blocked",
  LIFESPAN_POLICY: "ado_pat_lifespan_policy",
  CONSENT_NEEDED: "ado_pat_consent_needed",
  MINT_REFUSED: "ado_pat_mint_refused",
} as const;

/** The token-creation reason a caught error carries, "" when it carries none. */
export function adoPatRefusalReason(e: unknown): string {
  const reasons: string[] = Object.values(ADO_PAT_REASON);
  return e instanceof HttpError && reasons.includes(e.reason) ? e.reason : "";
}

export const adoPat = {
  // POST /api/v1/workspace-providers/git/{id}/org-check: runs the check with
  // the admin's own connection (two token creations, each revoked at once) on
  // the saved, enabled row, and returns the answer. Admin only, from a signed-in
  // browser session; a row that is off, unsaved or not the sign-in row is 404.
  async orgCheck(rowId: string): Promise<ADOOrgCheck> {
    const res = await wfetch(`/workspace-providers/git/${encodeURIComponent(rowId)}/org-check`, { method: "POST" });
    return asJson<ADOOrgCheck>(res);
  },

  // DELETE /api/v1/scm/azure-devops/connection (no server route yet): forgets
  // this person's stored sign-in and revokes the tokens of their runs in
  // progress. 204.
  async disconnect(): Promise<void> {
    const res = await wfetch("/scm/azure-devops/connection", { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new HttpError(res.status, await errText(res));
  },

  // GET /api/v1/runs/{id}/ado-tokens (no server route yet): every token the run
  // held, oldest first ([] for a run that holds none). The owner and admins only.
  async runTokens(runId: string): Promise<ADORunToken[]> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/ado-tokens`, { method: "GET" });
    return asJson<ADORunToken[]>(res);
  },

  // PUT /api/v1/me/scm/azure-devops/token: checks the pasted token belongs to
  // the caller, then seals it. The 2xx answer is the row's fresh /me/scm-access
  // entry, which the caller ignores in favour of one reload. Refusals carry the
  // reasons above (and ado_own_pat_token_invalid, _expiry_invalid,
  // _check_unavailable, _unknown_row, whose sentences render verbatim).
  async storeOwnToken(body: ADOOwnTokenBody): Promise<void> {
    const res = await wfetch("/me/scm/azure-devops/token", { method: "PUT", body: JSON.stringify(body) });
    if (!res.ok) await asJson<never>(res);
  },
};
