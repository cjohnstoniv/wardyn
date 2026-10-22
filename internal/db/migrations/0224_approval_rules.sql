-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- approval_rules holds the security tier's approval rules (types.ApprovalRule). The store bumps version on every
-- write, so an approval.decide row's rule_version names the exact text it matched. No row is read unless
-- WARDYN_APPROVAL_RULES is dry_run or on.
CREATE TABLE IF NOT EXISTS approval_rules (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    version    INTEGER NOT NULL CHECK (version >= 1),
    mode       TEXT NOT NULL CHECK (mode IN ('dry_run', 'enforce')),
    spec       JSONB NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
