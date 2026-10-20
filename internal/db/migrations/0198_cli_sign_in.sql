-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- CLI sign-in (wardyn login), off unless WARDYN_CLI_SIGN_IN is on.
--
-- cli_sign_ins is one browser-approved request. The row holds no secret: challenge is the PKCE S256
-- of a verifier only the CLI knows, public_key the device key's public half, and code_sha256 the hash
-- of the single-use code K the approval returns. The approver's identity snapshot (the columns an
-- api_tokens row carries, plus the approving session's revocation sequence and authority epoch) is
-- written at approval and read once, by the token call.
--
-- cli_sessions is one signed-in computer. secret_sha256 is the hash of the current wdc_ refresh secret
-- and generation counts its rotations: a refresh with a valid device proof and any other secret is a
-- reuse and ends the row. ended_at and end_reason are set together, once.
--
-- Three named cross-column CHECKs hold the pairings: cli_sessions_end_pair (ended_at with end_reason),
-- cli_sign_ins_loopback_port (a loopback sign-in has its port, a paste one none) and
-- api_tokens_cli_session_kind (a CLI token has its session, a personal token none). The closed-enum
-- guards count only enum-membership CHECKs: the migration-text guard reads `<col> IN (` lists, and the
-- live-catalog guard (enumCheckDefs in internal/db/migrations_check_pg_test.go) skips a CHECK over
-- several columns that holds no list for the column. So each enum column here keeps exactly one list.
--
-- A create deletes requests that expired over an hour ago (store.PG.CreateCLISignIn), so cli_sign_ins
-- holds about the last hour.
--
-- api_tokens gains kind (api, cli_access, cli_admin) and cli_session_id. A CLI token belongs to its
-- session and stops authenticating when the session ends (the token read joins it), and a personal
-- token never has one.
--
-- ADDITIVE ONLY: two new tables and two new api_tokens columns whose defaults leave every existing
-- token a personal one. A downgrade stops reading them.
CREATE TABLE IF NOT EXISTS cli_sessions (
    id                  UUID PRIMARY KEY,
    principal           TEXT NOT NULL,
    email               TEXT NOT NULL DEFAULT '',
    role                TEXT NOT NULL,
    user_type           TEXT NOT NULL,
    groups              JSONB,
    groups_truncated    BOOLEAN,
    identity_stamped_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    rev_seq             BIGINT NOT NULL DEFAULT 0,
    authority_epoch     BIGINT NOT NULL DEFAULT 0,
    machine             TEXT NOT NULL,
    store               TEXT NOT NULL CHECK (store IN ('keychain', 'file')),
    public_key          BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    secret_sha256       TEXT NOT NULL UNIQUE,
    generation          BIGINT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_refresh_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    ended_at            TIMESTAMPTZ,
    end_reason          TEXT CHECK (end_reason IN ('signed_out', 'revoked', 'sessions_cut', 'reuse_detected', 'expired', 'idle', 'deactivated')),
    CONSTRAINT cli_sessions_end_pair CHECK ((ended_at IS NULL) = (end_reason IS NULL))
);

CREATE INDEX IF NOT EXISTS cli_sessions_live_principal_idx ON cli_sessions (principal) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS cli_sessions_live_email_idx ON cli_sessions (lower(email)) WHERE ended_at IS NULL;

CREATE TABLE IF NOT EXISTS cli_sign_ins (
    id               UUID PRIMARY KEY,
    user_code        TEXT NOT NULL,
    challenge        TEXT NOT NULL,
    port             INTEGER CHECK (port BETWEEN 1 AND 65535),
    machine          TEXT NOT NULL,
    public_key       BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    mode             TEXT NOT NULL CHECK (mode IN ('loopback', 'paste')),
    store            TEXT NOT NULL CHECK (store IN ('keychain', 'file')),
    admin            BOOLEAN NOT NULL DEFAULT false,
    session_id       UUID REFERENCES cli_sessions (id) ON DELETE CASCADE,
    request_addr     TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    decision         TEXT CHECK (decision IN ('approved', 'denied')),
    decided_at       TIMESTAMPTZ,
    principal        TEXT,
    email            TEXT,
    role             TEXT,
    user_type        TEXT,
    groups           JSONB,
    groups_truncated BOOLEAN,
    rev_seq          BIGINT,
    authority_epoch  BIGINT,
    authorized_at    TIMESTAMPTZ,
    code_sha256      TEXT,
    code_expires_at  TIMESTAMPTZ,
    redeemed_at      TIMESTAMPTZ,
    CHECK (admin = (session_id IS NOT NULL)),
    CONSTRAINT cli_sign_ins_loopback_port CHECK ((mode = 'loopback') = (port IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS cli_sign_ins_expires_at_idx ON cli_sign_ins (expires_at);

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'api'
    CHECK (kind IN ('api', 'cli_access', 'cli_admin'));
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS cli_session_id UUID REFERENCES cli_sessions (id) ON DELETE CASCADE;
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_cli_session_kind;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_cli_session_kind CHECK ((kind = 'api') = (cli_session_id IS NULL));

CREATE INDEX IF NOT EXISTS api_tokens_cli_session_idx ON api_tokens (cli_session_id) WHERE cli_session_id IS NOT NULL;
