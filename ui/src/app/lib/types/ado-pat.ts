/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wire shapes the per-person Azure DevOps token console reads (#1428,
// #1430), each mirroring its Go struct: ADOPATAccess and adoRunToken
// (internal/api/ado_pat_console.go), adoOrgCheckResult (ado_pat_orgcheck.go),
// adoPATRefusal (ado_pat_refusal.go).
// SCMAccess's own token_mode, expires_on, max_days, token_scopes and git_only
// are in setup.ts. wire-parity.test.ts holds each of these to its Go struct.
import type { SCMAccess } from "./setup";

/** What /me/scm-access adds for a row that creates tokens (internal/api
 *  ADOPATAccess, embedded in SCMAccess). token_mode is SCMAccess's own field:
 *  "minted_pat" on a minted row, "own_pat" on an own-token row. */
export interface ADOPATAccess {
  /** minted_pat: the newest token Wardyn created for this person, for the card. */
  last_token?: { created_at: string; revoked_at?: string };
  /** What a run gets when its policy names nothing: the row's default profile. */
  default_profile?: string[];
}

/** SCMAccess as the token console reads it. On a minted row `expired_signin`
 *  carries the causes permissions_missing, blocked and ado_pat_needs_console_app
 *  beside ended and consent_needed; on an own-token row token_expired. */
export type SCMAccessPAT = SCMAccess & ADOPATAccess;

/** One token a run held (GET /runs/{id}/ado-tokens; internal/api adoRunToken):
 *  a row of ado_run_pats, never the token value or its authorization id. */
export interface ADORunToken {
  created_at: string;
  valid_to: string;
  /** The token's scope names as stored, e.g. ["vso.code", "vso.project"]. */
  scope: string[];
  /** Set once the record is closed. With revoke_reason "expired" it is when the
   *  record was closed, not a revoke. */
  revoked_at?: string;
  /** L2's reason as written: run_end, kill, pause, drift, disconnect, offboarding,
   *  upstream_401, sweep or expired. */
  revoke_reason?: string;
  /** The record was closed with a failed revoke on it: the token lives to
   *  valid_to on its own. */
  revoke_failed?: boolean;
}

/** POST /workspace-providers/git/{id}/org-check's answer (internal/api
 *  adoOrgCheckResult). Empty token_life or lifespan means that step was not
 *  reached; lifespan "unknown" means Azure DevOps said nothing that tells. */
export interface ADOOrgCheck {
  checked_at: string;
  organisation: string;
  pat_max_hours: number;
  permissions: "granted" | "missing";
  token_life?: "accepted" | "refused";
  /** The reason (ado_pat_*) canary 1 was refused with. */
  refusal?: string;
  lifespan?: "on" | "off" | "unknown";
  /** lifespan "unknown": what Azure DevOps answered to the year-long probe
   *  (its patTokenError or error key), for the "couldn't tell" line. */
  lifespan_error?: string;
  /** Canaries Wardyn created and could not revoke. */
  unrevoked?: string[];
}

/** GET /workspace-providers/git/{id}/ado-pat-refusal's answer (internal/api
 *  adoPATRefusal): the newest launch in the last seven days the organisation's
 *  token-creation policy refused, as the refused person's email and the time.
 *  Admin only; 204 (no body) when there is none. */
export interface ADOPATRefusal {
  person: string;
  at: string;
}

/** What the own-token dialog sends: PUT /me/scm/azure-devops/token's body. */
export interface ADOOwnTokenBody {
  /** The row's address, exactly as /me/scm-access names it (SCMAccess.org). */
  org: string;
  token: string;
  /** YYYY-MM-DD, the expiry the person chose in Azure DevOps. */
  expires_on: string;
}
