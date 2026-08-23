-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-user API tokens (0.6 TIER-B #1): a long-lived bearer credential a HUMAN
-- mints for their own scripts/CI, so automation stops sharing the single
-- deployment-wide WARDYN_ADMIN_TOKEN. The whole point is that a token carries
-- the identity of the human who minted it — grants, RBAC and run ownership then
-- bind to that human for free, because the auth branch republishes exactly the
-- context a verified SSO session publishes (see apiTokenAuth in
-- internal/api/apitokens.go).
--
-- token_sha256 holds hex(sha256(raw token)) and NEVER the raw value, the same
-- rule 0032 imposed on attach_tickets: a live-DB reader (a read-only reporting
-- role, a hot standby, a pg_dump in a backup bucket) must not be able to lift a
-- usable credential off a row. The plaintext is returned to its creator ONCE, at
-- create time, and is unrecoverable afterwards. UNIQUE is both the "one row per
-- credential" constraint and the index the auth-time lookup rides.
--
-- IDENTITY SNAPSHOT (email, role, groups): stamped from the CREATING SESSION,
-- exactly the way 0034 stamps attach_tickets.role and 0043 stamps
-- ssh_public_keys.role. A bearer token carries no ID token, so there is nothing
-- to re-derive a role or a group set from at request time; without the snapshot
-- the auth branch could only publish the sub, and every group-subject grant —
-- INCLUDING A DENY — would silently fail to match for token calls. A deny that
-- evaporates when the caller switches credential is a breach, not a degradation,
-- so the snapshot is required, not an optimization.
--
-- CEILING, stated here because the columns cannot state it themselves: a STAMP
-- is not a live check. A human demoted from admin to member keeps the role their
-- outstanding tokens were minted under until those tokens are REVOKED — the same
-- bound 0043 documents for SSH keys. Revocation, not re-derivation, is the
-- remediation (DELETE /api/v1/tokens/{id}, admin, revokes anyone's).
--
-- groups is JSONB and NULLABLE because nil and empty are DIFFERENT (see
-- oidcGroupsCtxKey in internal/api/http.go): NULL means the creating session had
-- no answerable group identity at all (a pre-0.6 cookie), which the resolver must
-- read as "snapshot unavailable", while '[]' means the IdP genuinely sent none.
-- Collapsing them here would silently withhold every group grant a token holder
-- has, unexplainably from the admin side.
--
-- revoked_at is a SOFT delete: the row survives revocation so the audit trail
-- (`token.create` / `token.revoke`, docs/AUDIT-ACTIONS.md) still resolves the
-- token id it names, and so a revoked token's name can never be silently reused
-- to impersonate a retired credential. The auth lookup filters on
-- `revoked_at IS NULL`, which is what makes a revoked token indistinguishable
-- from an unknown one at the boundary — no oracle.
--
-- last_used_at is NULL until first use and is best-effort thereafter (the auth
-- branch ignores the update's error; failing to record a touch must never fail
-- an otherwise-valid request).
--
-- No CHECK on role, matching 0034/0043: the two values are Go constants
-- (oidc.RoleAdmin / oidc.RoleMember) written at exactly one call site, and
-- isOperator compares for equality with RoleAdmin — any other value is inert
-- (fail closed), never a privilege. DEFAULT 'member' is the fail-closed value
-- for the same reason 0043 chose it.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           UUID PRIMARY KEY,
    principal    TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL DEFAULT 'member',
    groups       JSONB,
    name         TEXT NOT NULL DEFAULT '',
    token_sha256 TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- Serves the self-service list (GET /me/tokens, WHERE principal = $1). The
-- auth-time lookup needs no index of its own — it rides token_sha256's UNIQUE.
CREATE INDEX IF NOT EXISTS api_tokens_principal_idx ON api_tokens (principal);
