-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Key domains. A domain is a tenant of the deployment's key service, declared
-- in WARDYN_KEY_DOMAINS_FILE and proven at boot; the database never declares
-- one. These tables only say which domain a subject's NEXT principal-key
-- generation is wrapped under.
--
--   * key_domain_assignments: the governance subject vocabulary
--     (0052_governance_profiles). 'all' keeps subject '' as it does there.
--     domain is checked against the file in code, not in SQL, because the file
--     is the authority.
--   * key_domain_login_groups: the group facts of a person's last verified
--     login, which is where a group assignment reads membership. truncated is
--     the login's own bit: a snapshot that lost groups cannot say which domain
--     a person is in, so resolution refuses rather than guess.
CREATE TABLE IF NOT EXISTS key_domain_assignments (
    subject_type TEXT        NOT NULL CHECK (subject_type IN ('user', 'group', 'all')),
    subject      TEXT        NOT NULL,
    domain       TEXT        NOT NULL CHECK (domain <> ''),
    set_by       TEXT        NOT NULL,
    set_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (subject_type, subject)
);

CREATE TABLE IF NOT EXISTS key_domain_login_groups (
    principal   TEXT        PRIMARY KEY CHECK (principal <> ''),
    groups      JSONB       NOT NULL DEFAULT '[]',
    truncated   BOOLEAN     NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A reassignment applies to the NEXT generation and never moves an old one, so
-- the generation a write used before it stays readable beside the new one.
-- superseded_at marks a generation that is still readable (destroyed_at stays
-- NULL, wrapped_key stays set) but is no longer the one a write uses; the
-- one-current index replaces 0108's one-live index.
ALTER TABLE principal_keys ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ;

DROP INDEX IF EXISTS principal_keys_one_live;
CREATE UNIQUE INDEX IF NOT EXISTS principal_keys_one_current
    ON principal_keys (owner, purpose) WHERE destroyed_at IS NULL AND superseded_at IS NULL;
