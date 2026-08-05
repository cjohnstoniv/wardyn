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
  created_at: string;
}
