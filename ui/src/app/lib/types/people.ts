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
