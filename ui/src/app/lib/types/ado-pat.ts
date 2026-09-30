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

/** What /me/scm-access adds on a row whose token_mode is minted_pat or own_pat. */
export interface ADOPATAccess {
  token_mode?: ADOTokenMode;
  /** minted_pat: the last token Wardyn created for this person, for the card. */
  last_token?: { created_at: string; revoked_at?: string };
  /** own_pat: when the pasted token stops being used (RFC 3339). */
  expires_at?: string;
  /** own_pat: the furthest expiry the admin allows, in days. */
  pat_max_days?: number;
  /** own_pat: the Azure DevOps scopes the token needs (`vso.code_write`), off
   *  the row's ceiling. */
  own_scopes?: string[];
  /** own_pat on an Azure DevOps Server row: git only, no Entra sign-in. */
  server?: boolean;
  /** What a run gets when its policy names nothing: the row's default profile. */
  default_profile?: string[];
}

/** SCMAccess as the token console reads it. `state` also takes "blocked" (the
 *  organisation restricts who may create tokens) and "expired" (an own token
 *  past its expiry), beside the states the mirror lists. */
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

/** The organisation-settings check's answer, and the admin-facing refusals
 *  the server has seen since. Every field is absent until it is known. */
export interface ADOTokenHealth {
  checked_at?: string;
  permissions?: "granted" | "missing";
  lifespan?: "on" | "off" | "too_long";
  /** lifespan on: the hours the check saw accepted (the row's longest life);
   *  too_long: the longest life it saw accepted, when it knows one. */
  lifespan_hours?: number;
  /** The person Azure DevOps last refused a token for on the organisation's
   *  create policy; absent when nobody was. */
  blocked_person?: string;
}

/** What the own-token dialog sends. */
export interface ADOOwnTokenBody {
  token: string;
  /** YYYY-MM-DD, the expiry the person chose in Azure DevOps. */
  expires_on: string;
}
