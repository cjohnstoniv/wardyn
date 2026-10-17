-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The leaver work (SCIM) as a durable job. A suspension is several steps, each
-- idempotent and each safe to run again: the session cutoff and deactivation
-- (one transaction), then the credential sweep per principal, then the kill of
-- every run per principal, then the audit rows. One row per step and target
-- records what is still pending, so a retry or a restart resumes the failed tail
-- (including a run already KILLED whose teardown failed) instead of starting
-- over, and the request that asked for it answers 5xx until every row is done.
--
-- kind says which job a row belongs to; only 'suspend' is written today. state
-- is 'pending' until the step is confirmed. detail holds the step's counts
-- (tokens revoked, keys deleted, runs killed), added up across attempts.
-- No row holds a secret.
CREATE TABLE IF NOT EXISTS deprovision_jobs (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id UUID        NOT NULL REFERENCES principal_identities (id),
    kind        TEXT        NOT NULL CHECK (kind IN ('suspend', 'purge', 'group_remove')),
    step        TEXT        NOT NULL,
    target      TEXT        NOT NULL DEFAULT '',
    state       TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done')),
    attempts    INTEGER     NOT NULL DEFAULT 0,
    last_error  TEXT        NOT NULL DEFAULT '',
    detail      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (identity_id, kind, step, target)
);

-- A person an admin set up before their first sign-in, deactivated alongside
-- their identity row so the People view and PrincipalFor can say so. NULL is
-- active; the sign-in gate itself reads principal_identities, not this column.
ALTER TABLE people ADD COLUMN IF NOT EXISTS deactivated_at TIMESTAMPTZ;
