/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The per-person Azure DevOps token routes (#1428, #1430). Paths and shapes
// are the plan's; the server lanes that answer them build in parallel with
// this file, so a route that is not on a daemon yet fails like any other
// missing route and the card that asked stays as it was.
import type { ADOOwnTokenBody, ADORunToken, ADOTokenHealth } from "../types/ado-pat";
import { asJson, errText, HttpError, wfetch } from "./core";

// The machine-readable classes an own-token store answers a 422 with; the
// dialog keeps each under the field it is about.
export const OWN_TOKEN_MISMATCH = "ado_pat_identity_mismatch";
export const OWN_TOKEN_TOO_LONG = "ado_pat_expiry_too_long";
export const OWN_TOKEN_REJECTED = "ado_pat_rejected";

// The reasons a personal-access-token creation is refused with (internal/api
// reasons.go, docs/sdk.md), as the envelope's `reason`. Each maps to console
// copy in ado-pat-display.ts's patRefusalNote; the server's own sentence still
// renders verbatim in the launch strip.
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
  // GET /api/v1/scm/azure-devops/org-check: the last check's answer ({} when
  // none has run). Admin only.
  async health(): Promise<ADOTokenHealth> {
    const res = await wfetch("/scm/azure-devops/org-check", { method: "GET" });
    return asJson<ADOTokenHealth>(res);
  },

  // POST /api/v1/scm/azure-devops/org-check: runs the check with the admin's
  // own connection (two token creations, both revoked at once) and returns the
  // fresh answer.
  async orgCheck(): Promise<ADOTokenHealth> {
    const res = await wfetch("/scm/azure-devops/org-check", { method: "POST" });
    return asJson<ADOTokenHealth>(res);
  },

  // DELETE /api/v1/scm/azure-devops/connection: forgets this person's stored
  // sign-in and revokes the tokens of their runs in progress. 204.
  async disconnect(): Promise<void> {
    const res = await wfetch("/scm/azure-devops/connection", { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new HttpError(res.status, await errText(res));
  },

  // GET /api/v1/runs/{id}/ado-tokens: every token the run held, oldest first
  // ([] for a run that holds none). The owner and admins only.
  async runTokens(runId: string): Promise<ADORunToken[]> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/ado-tokens`, { method: "GET" });
    return asJson<ADORunToken[]>(res);
  },

  // PUT /api/v1/scm/azure-devops/own-token: checks the pasted token belongs to
  // the caller, then seals it. 422 with reason ado_pat_identity_mismatch,
  // ado_pat_expiry_too_long or ado_pat_rejected when it does not.
  async storeOwnToken(body: ADOOwnTokenBody): Promise<void> {
    const res = await wfetch("/scm/azure-devops/own-token", { method: "PUT", body: JSON.stringify(body) });
    if (!res.ok) await asJson<never>(res);
  },
};
