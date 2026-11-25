/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/people — mirrors internal/types PersonSummary and PersonList (the admin People page).
export interface PersonSummary {
  principal: string;
  email?: string;
  issuer_kind: string;
  pre_created: boolean;
  first_signed_in_at?: string;
  last_signed_in_at?: string;
  deactivated_at?: string;
  role?: string;
  active_sessions: number;
  api_tokens: number;
  ssh_keys: number;
  credentials: number;
  active_runs: number;
  // A portal person only: the issuer their portal registered, and the internal address its binding
  // fetches keys from (absent when the issuer's own address is used).
  issuer?: string;
  internal_address?: string;
}

export interface PersonList {
  people: PersonSummary[];
  next_cursor?: string;
}

/** One of a person's API tokens, as GET /people/{principal}/tokens lists it: metadata, never a value. */
export interface PersonToken {
  id: string;
  name: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  expires_at?: string;
}

/** One portal a portal person has used, as GET /people/{principal}/portals lists it. A revoked portal stays, with revoked_at set. */
export interface PortalUsed {
  delegate: string;
  name: string;
  run_in_under?: string;
  revoked_at?: string;
}

/** A portal person as the exact lookup and the portals read answer: the state, the email the portal gave, and the portals used. */
export interface PortalPersonView {
  principal: string;
  state: "active" | "deactivated" | "purged";
  display_email: string;
  deactivated_at?: string;
  purge_after?: string;
  purged_at?: string;
  portals_used: PortalUsed[];
}
