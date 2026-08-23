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
  // Stamped from the registering session's role and NEVER re-stamped
  // (migration 0043): "admin" here means the key carries the SSH gateway's
  // admin override — it reaches runs its holder does not own — and it keeps
  // carrying it after a demotion, until the key is deleted and registered
  // again. Keys registered before 0.6 are backfilled "member". docs/SSH.md
  // §Bounds; the console shows it because otherwise nobody can see it.
  role: string;
  created_at: string;
}
