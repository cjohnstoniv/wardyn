-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run masking manifest: the exact bytes of every rendering of a secret a
-- run received, committed at dispatch before the sandbox can see a value, so a
-- restarted or second wardynd masks what the run actually holds rather than
-- what a re-resolved secret name says today (a rotated value would pass a
-- naive restart test and still leak the old one).
--
-- run_mask_manifest has one row per dispatched run. complete is set last, once
-- every value is committed: a run is covered only while its row is complete and
-- not fenced. revision moves on every append, completion and fence, so a replica
-- holding an older copy notices the change at its next admission check.
-- fenced_at is the durable erasure fence every replica honours: it is set in the
-- same transaction that deletes the subject's value rows, and a fenced run is
-- never covered again. owner is the run owner's secret subject, never ''.
--
-- run_mask_values holds one sealed rendering per row, sealed with the owner's
-- 'cred' subject key from principal_keys (AES-256-GCM, AAD binding the run id,
-- the ordinal and key_version), so destroying that key leaves them undecryptable.
-- owner repeats the manifest's so a subject's rows are deleted without a join.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on
-- run_mask_manifest and SELECT, INSERT, DELETE on run_mask_values.
CREATE TABLE IF NOT EXISTS run_mask_manifest (
    run_id     UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    owner      TEXT        NOT NULL CHECK (owner <> ''),
    complete   BOOLEAN     NOT NULL DEFAULT false,
    revision   INT         NOT NULL DEFAULT 0,
    fenced_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS run_mask_manifest_owner_idx ON run_mask_manifest (owner);

CREATE TABLE IF NOT EXISTS run_mask_values (
    run_id      UUID  NOT NULL REFERENCES run_mask_manifest(run_id) ON DELETE CASCADE,
    ordinal     INT   NOT NULL CHECK (ordinal >= 0),
    owner       TEXT  NOT NULL CHECK (owner <> ''),
    key_version INT   NOT NULL CHECK (key_version >= 1),
    sealed      BYTEA NOT NULL,
    PRIMARY KEY (run_id, ordinal)
);

CREATE INDEX IF NOT EXISTS run_mask_values_owner_idx ON run_mask_values (owner);
