-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The live state a second wardynd must see (ha-l2.4): a run's exec output tail, its attach
-- lease, its Azure DevOps run token and a person's sign-in end counter.
--
-- run_output_chunks is the live exec tail. Each row is bytes the dispatching replica's masker
-- already passed (bytes_masked: raw bytes never reach this table), numbered per run by seq so a
-- read on any replica concatenates them in order. The writer keeps about one tail of chunks per
-- run, deleting the oldest as it appends, and the transaction that writes a run's final row in
-- run_outputs deletes them. Writers and readers check run_output_erasures (0119) in their own
-- transaction, so an erased run's chunks are never written again.
--
-- run_attach_leases is the one writer slot of a run's shared terminal: a row per run naming the
-- holder (holder_id is the lease's identity: input and resize are fenced by it), the replica
-- that serves its socket, and when the lease lapses unless its replica renews it. The database's
-- clock decides expiry. epoch counts takeovers of the row.
--
-- ado_run_pat_state is the `minted_pat` run's current token, which each replica used to hold in
-- memory: the value sealed with the run owner's 'cred' subject key (AES-256-GCM, AAD binding the
-- run id, the rendering and the key version), so destroying that key leaves it undecryptable,
-- and what the token was built from. A writer checks the run's masking manifest (0109) is not
-- fenced in its own transaction, so a person's erasure cannot be followed by a late write.
--
-- ado_signin_ends counts, per person, the disconnects and erases of their Azure DevOps
-- sign-in: gen moves at each end's start and finish, active counts the ends still running.
-- The row holds no secret.
--
-- Split-role installs grant the app role SELECT, INSERT, DELETE on run_output_chunks, SELECT,
-- INSERT, UPDATE, DELETE on run_attach_leases and ado_run_pat_state, and SELECT, INSERT,
-- UPDATE on ado_signin_ends.
CREATE TABLE IF NOT EXISTS run_output_chunks (
    run_id       UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    seq          BIGINT      NOT NULL CHECK (seq >= 1),
    replica      TEXT        NOT NULL,
    bytes_masked BYTEA       NOT NULL,
    at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS run_attach_leases (
    run_id     UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    holder_id  UUID        NOT NULL,
    replica    TEXT        NOT NULL,
    principal  TEXT        NOT NULL,
    source     TEXT        NOT NULL,
    since      TIMESTAMPTZ NOT NULL,
    epoch      BIGINT      NOT NULL DEFAULT 1,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS ado_run_pat_state (
    run_id           UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    owner            TEXT        NOT NULL CHECK (owner <> ''),
    authorization_id TEXT        NOT NULL DEFAULT '',
    scope            TEXT        NOT NULL DEFAULT '',
    valid_to         TIMESTAMPTZ,
    key_version      INT         CHECK (key_version IS NULL OR key_version >= 1),
    sealed           BYTEA,
    capabilities     TEXT        NOT NULL DEFAULT '',
    paused           BOOLEAN     NOT NULL DEFAULT false,
    forced_at        TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((sealed IS NULL) = (key_version IS NULL))
);

CREATE INDEX IF NOT EXISTS ado_run_pat_state_owner_idx ON ado_run_pat_state (owner);

CREATE TABLE IF NOT EXISTS ado_signin_ends (
    owner      TEXT        PRIMARY KEY CHECK (owner <> ''),
    gen        BIGINT      NOT NULL DEFAULT 0,
    active     INT         NOT NULL DEFAULT 0 CHECK (active >= 0),
    reason     TEXT        NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
