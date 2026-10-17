-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Registration extends the runner identity table; it does not own run placement.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS org_url_sha256 TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS runner_registration_tokens (
    id UUID PRIMARY KEY,
    owner TEXT NOT NULL,
    minted_by TEXT NOT NULL,
    token_sha256 TEXT NOT NULL UNIQUE,
    org_url_sha256 TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS runner_registration_tokens_expiry_idx ON runner_registration_tokens (expires_at)
    WHERE consumed_at IS NULL;
