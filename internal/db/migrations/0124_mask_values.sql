-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The shared masking registry: every value wardynd masks after dispatch, so any
-- replica masks what any other registered, and a restarted one masks it too.
--
-- mask_values has two buckets (the 'bucket' column):
--   per_run       a value registered for one run AFTER dispatch (a minted token,
--                 an injected key). The run's dispatch-time renderings are in
--                 run_mask_values (0109), never here. run_id references
--                 agent_runs, so a deleted run takes its rows with it.
--   owner_global  a credential's current and retired values, masked on every
--                 run: the AWS SSO and Azure DevOps sign-in tokens. 'owner' is
--                 the person the credential belongs to and 'name' the credential.
-- A shared (operator-namespace) global is not stored: none exists today, and
-- one would be sealed under a platform key, which this migration does not add.
--
-- Every sealed value is under its owner's 'cred' subject key (principal_keys),
-- AES-256-GCM, with an AAD that binds the bucket, the row id, the owner, the
-- run or credential name and the key version, so destroying that key leaves the
-- rows undecryptable. digest is an HMAC under that same key version (never a
-- bare hash of the value), so two replicas writing one value land on one row
-- without the table holding anything that identifies the value once the key is
-- gone.
--
-- gen is the generation cursor. A transaction that registers or evicts takes it
-- from mask_gen with UPDATE ... SET gen = gen + 1 RETURNING gen. The UPDATE's
-- row lock is held until commit, so transactions commit in generation order and
-- a reader that has seen generation g has seen every generation below it: the
-- cursor is a committed prefix. A plain sequence would not be.
--
-- An eviction is a tombstone: tombstone is set and sealed, digest, owner and
-- name are cleared in ONE statement, so no row for an evicted value holds
-- ciphertext and a replica reading above its cursor still learns the row is
-- gone. retired_at on a tombstone, when set, says the value stopped being current
-- then and may stay masked in a replica's cache until its grace ends (a deleted
-- credential); unset, a replica drops it at once (a swept value, an erased
-- person's). updated_at is when the row last changed.
-- The retention pass deletes tombstones past its horizon and records the highest
-- generation it deleted in mask_gen.pruned; a replica whose cursor is below
-- that reloads the whole table instead of reading a gap.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on
-- mask_values, SELECT and UPDATE on mask_gen, and DELETE on run_mask_manifest
-- (0109), which the retention pass uses for runs past their grace.
CREATE TABLE IF NOT EXISTS mask_gen (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    gen       BIGINT  NOT NULL DEFAULT 0,
    pruned    BIGINT  NOT NULL DEFAULT 0
);

INSERT INTO mask_gen (singleton) VALUES (true) ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS mask_values (
    id          UUID        PRIMARY KEY,
    bucket      TEXT        NOT NULL CHECK (bucket IN ('per_run', 'owner_global')),
    owner       TEXT        NOT NULL DEFAULT '',
    run_id      UUID        REFERENCES agent_runs(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL DEFAULT '',
    digest      BYTEA,
    key_version INT         CHECK (key_version IS NULL OR key_version >= 1),
    sealed      BYTEA,
    until       TIMESTAMPTZ,
    retired_at  TIMESTAMPTZ,
    tombstone   BOOLEAN     NOT NULL DEFAULT false,
    gen         BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (tombstone = (sealed IS NULL)),
    CHECK (tombstone OR (digest IS NOT NULL AND key_version IS NOT NULL AND owner <> '')),
    CHECK ((bucket = 'per_run') = (run_id IS NOT NULL)),
    CHECK (bucket = 'per_run' OR tombstone OR name <> '')
);

-- One live row per value: a second replica registering the same value finds it.
CREATE UNIQUE INDEX IF NOT EXISTS mask_values_digest_idx ON mask_values (bucket, digest) WHERE NOT tombstone;
CREATE INDEX IF NOT EXISTS mask_values_gen_idx ON mask_values (gen);
CREATE INDEX IF NOT EXISTS mask_values_owner_idx ON mask_values (owner) WHERE NOT tombstone;
CREATE INDEX IF NOT EXISTS mask_values_run_idx ON mask_values (run_id) WHERE run_id IS NOT NULL;
