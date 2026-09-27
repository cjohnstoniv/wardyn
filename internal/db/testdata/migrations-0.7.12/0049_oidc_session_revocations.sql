-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- D16: OIDC human sessions are stateless signed cookies (see
-- internal/auth/oidc's package doc, "Session storage") — there is no
-- server-side session row to delete on logout/role-demotion/IdP-disable, so
-- there was no way to make a still-unexpired session stop working before its
-- own Expiry. This table is the revoke-a-human-now lever: each row is a
-- CUTOFF time, and internal/auth/oidc's Middleware treats any session whose
-- IssuedAt is at-or-before the applicable cutoff as invalid on its very next
-- request.
--
-- sub is the OIDC "sub" claim a revoke targets; the empty string '' is the
-- reserved GLOBAL sentinel — a revoke-all writes ONLY that row, and
-- IsSessionRevoked checks both the caller's own sub row and the global row,
-- whichever cutoff is later. One row per sub (UPSERT on repeat revokes), so
-- this table never grows with login volume — only with how many DISTINCT
-- principals have ever been revoked.
CREATE TABLE IF NOT EXISTS oidc_session_revocations (
    sub        TEXT PRIMARY KEY,
    revoked_at TIMESTAMPTZ NOT NULL
);
