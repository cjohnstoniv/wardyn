/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A human's registered SSH gateway public key (GET/POST/DELETE
// /api/v1/me/ssh-keys) — mirrors internal/types.SSHPublicKey. Distinct from
// any git-credential "ssh_key" grant; this is the SSH gateway's own trust
// root, self-service and scoped to the signed-in human's own principal.
export interface SSHPublicKey {
  fingerprint: string; // "SHA256:…" — the PK, computed server-side
  principal: string;
  name: string;
  public_key: string;
  // Stamped from the registering session's role and RE-stamped on every OIDC
  // login for this principal's keys (migration 0046): "admin" here means the
  // key carries the SSH gateway's admin override — it reaches runs its
  // holder does not own — until it expires on its own (role_checked_at older
  // than WARDYN_SSH_ROLE_TTL) or the holder is demoted at their next login;
  // deleting and re-registering the key drops the override immediately too.
  // Keys registered before 0.6 are backfilled "member". docs/SSH.md §Bounds;
  // the console shows it because otherwise nobody can see it.
  role: string;
  role_checked_at?: string; // when `role` was last (re-)stamped; absent/undefined for a pre-0046 row
  created_at: string;
}
