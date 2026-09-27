-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Delegated run management (#1142). A registered portal exchanges the
-- signed-in person's own identity-provider token for a short delegated token
-- that acts for that person, and every act on it is recorded as delegation.
--
--   * delegates         — the registered portals, one credential each.
--   * delegated_tokens  — the live tokens they were handed: ten minutes, no
--                         refresh, swept on the next mint once expired.
--
-- Both credential columns store only a SHA-256 hash of the bearer, matching
-- api_tokens (0045) and devices (0066): a live-DB reader must not be able to
-- lift a usable credential off a row.

CREATE TABLE IF NOT EXISTS delegates (
    id                UUID        PRIMARY KEY,
    name              TEXT        NOT NULL,
    -- The portal's own client id at the identity provider: a subject token
    -- must have been issued to it, or for Wardyn at its request.
    idp_client_id     TEXT        NOT NULL,
    -- The one group a person must be in for this portal to act for them.
    scope_group       TEXT        NOT NULL,
    credential_sha256 TEXT        NOT NULL UNIQUE,
    registered_by     TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at      TIMESTAMPTZ,
    -- A soft delete: the row survives so an audit row's data.via still
    -- resolves. Every delegated-token lookup joins on revoked_at IS NULL, so
    -- revoking the portal ends its outstanding tokens on their next request.
    revoked_at        TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS delegated_tokens (
    id               UUID        PRIMARY KEY,
    delegate_id      UUID        NOT NULL REFERENCES delegates (id),
    token_sha256     TEXT        NOT NULL UNIQUE,
    principal        TEXT        NOT NULL,
    email            TEXT        NOT NULL DEFAULT '',
    user_type        TEXT        NOT NULL,
    groups           JSONB,
    groups_truncated BOOLEAN     NOT NULL,
    -- On the database clock, like api_tokens.created_at: the person's
    -- session cutoff (oidc_session_revocations.revoked_at) is compared to it.
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS delegated_tokens_expires_at ON delegated_tokens (expires_at);

-- An attach ticket minted on the delegated lane names the portal and the
-- delegated token, so the ticket lane (which runs no auth middleware) can
-- replay them onto session.attach and the UI gateway's entry rows.
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS via_delegate UUID;
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS via_grant UUID;

-- The portal a run was launched through; NULL for every other run.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS created_via UUID;
