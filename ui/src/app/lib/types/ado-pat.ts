/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wire shapes the per-person Azure DevOps token console reads (#1428,
// #1430). The server lanes that produce them (mint, run lifecycle, own token,
// retire) build in parallel with this one, so these are the shapes the plan
// names, kept apart from SCMAccess and the other Go mirrors until each Go
// struct exists on the base: the wire-parity probes compare a TS interface
// with its Go struct field for field, and a field with no Go twin would fail
// them. When a Go struct lands, fold its interface into the mirrored file and
// add its parity row.
import type { ADOTokenMode } from "./site";
import type { SCMAccess } from "./setup";

/** What /me/scm-access adds for a row that creates tokens or takes a pasted one.
 *
 *  token_mode, expires_on, max_days and token_scopes are internal/api's
 *  SCMAccess fields from the own-token lane (#1430): they are set on an own_pat
 *  row and on no other, so when that lane merges these four move into the
 *  mirrored SCMAccess (setup.ts), where its parity probe covers them.
 *  token_mode must ALSO read "minted_pat" on a minted row: without it a member's
 *  console cannot tell a row that creates a token per run from the Entra
 *  sign-in, and the card, the New Run line and the launch note all key on it.
 *  last_token and default_profile have no server source yet. */
export interface ADOPATAccess {
  token_mode?: ADOTokenMode;
  /** own_pat: the date (YYYY-MM-DD) the person said their token expires. */
  expires_on?: string;
  /** own_pat: the furthest expiry, in days from today, the admin allows. */
  max_days?: number;
  /** own_pat: what to tick on Azure DevOps' own token page, in its own wording. */
  token_scopes?: string[];
  /** minted_pat: the last token Wardyn created for this person, for the card. */
  last_token?: { created_at: string; revoked_at?: string };
  /** What a run gets when its policy names nothing: the row's default profile. */
  default_profile?: string[];
}

/** SCMAccess as the token console reads it. On a minted row `expired_signin`
 *  carries the causes permissions_missing, blocked and ado_pat_needs_console_app
 *  beside ended and consent_needed; on an own-token row token_expired. */
export type SCMAccessPAT = SCMAccess & ADOPATAccess;

/** One token a run held: a row of ado_run_pats, never the token value. */
export interface ADORunToken {
  created_at: string;
  valid_to: string;
  revoked_at?: string;
  /** ado_run_pats.revoke_reason: run_end, kill, pause, renewal, widen, drift,
   *  disconnect or sweep. */
  revoke_reason?: string;
  /** Set when a revoke was abandoned: the token expires at valid_to on its own. */
  revoke_failed?: boolean;
  /** Set on the live token when a renewal failed: it stops working at valid_to. */
  renewal_failed?: boolean;
  /** Set on a token created by a widening: the capabilities it added. */
  added_capabilities?: string[];
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

/** What the own-token dialog sends: PUT /me/scm/azure-devops/token's body. */
export interface ADOOwnTokenBody {
  /** The row's address, exactly as /me/scm-access names it (SCMAccess.org). */
  org: string;
  token: string;
  /** YYYY-MM-DD, the expiry the person chose in Azure DevOps. */
  expires_on: string;
}
