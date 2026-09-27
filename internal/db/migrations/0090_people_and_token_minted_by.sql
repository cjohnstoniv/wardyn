-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- people is a person an admin created or confirmed before their first sign-in
-- (#1157), keyed by the identity provider's subject exactly as the id_token's
-- `sub` will carry it, which is the principal a sign-in resolves to. The email
-- is the admin's assertion and never a key: a sign-in attaches by subject
-- alone. first_signed_in_at is stamped by the sign-in hook.
CREATE TABLE IF NOT EXISTS people (
    principal          TEXT PRIMARY KEY CHECK (principal <> ''),
    email              TEXT NOT NULL DEFAULT '',
    created_by         TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_signed_in_at TIMESTAMPTZ
);

-- One person per email, case-insensitively: two rows sharing one would let an
-- email-keyed role mapping or grant speak for both.
CREATE UNIQUE INDEX IF NOT EXISTS people_email_lower_uniq ON people (lower(email)) WHERE email <> '';

-- minted_by names the admin who minted a token for someone else; NULL is a
-- token its owner minted for themselves.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS minted_by TEXT;
