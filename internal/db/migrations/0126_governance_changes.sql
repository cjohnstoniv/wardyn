-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Governance writes held for a second human (WARDYN_GOVERNANCE_SECOND_HUMAN). With the switch on, a
-- covered write is stored here as a pending change instead of applied; a distinct authorised human
-- approves it, and only then does the target mutate, in one transaction with the state change.
--
-- target_kind has no CHECK: the closed set lives in one Go table (the 0052 precedent), so a later lane
-- adds a kind with no DDL. target_key is the target's natural key, and at most one change per target
-- is pending at a time. base_hash is the hash of the target as the proposer's reviewer saw it ('absent'
-- for a create) and deployment_hash the hash of the deployment default at proposal: an approval that
-- finds either changed is refused as stale. payload holds the validated request and diff the redacted
-- before/after the reviewer reads; neither carries a secret value.
--
-- proposed_by, proposed_by_email, decided_by and decided_by_email are a person's personal fields; the
-- audit_personal_fields erasure scope clears them. Decided rows stay as the governance history.
CREATE TABLE IF NOT EXISTS governance_changes (
    id                UUID PRIMARY KEY,
    target_kind       TEXT NOT NULL,
    op                TEXT NOT NULL CHECK (op IN ('create','update','delete','upsert','replace','set')),
    target_key        TEXT NOT NULL,
    payload           JSONB NOT NULL,
    diff              JSONB NOT NULL,
    base_hash         TEXT NOT NULL,
    deployment_hash   TEXT NOT NULL,
    proposed_by       TEXT NOT NULL,
    proposed_by_email TEXT NOT NULL DEFAULT '',
    proposed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    state             TEXT NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','applied','rejected','expired','stale')),
    decided_by        TEXT NOT NULL DEFAULT '',
    decided_by_email  TEXT NOT NULL DEFAULT '',
    decided_at        TIMESTAMPTZ,
    reason            TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS governance_changes_one_pending
    ON governance_changes (target_kind, target_key) WHERE state = 'pending';

CREATE INDEX IF NOT EXISTS governance_changes_state_expires
    ON governance_changes (state, expires_at);
