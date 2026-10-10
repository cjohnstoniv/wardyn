-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Runner pools (0.9): the organisation's catalogue of places a run may go, who
-- sits in each, who may use it, and the organisation's defaults. A pool is a
-- label for a choice and never authority: nothing here grants a runner, a
-- credential, a drive or a confinement class. A run's own pool binding is a later
-- migration's; this one stores only the catalogue.
--
-- hosting_type is fixed for the life of a pool. A deleted pool is a tombstone
-- (state 'deleted'): its row stays so a run bound to it keeps its history, its
-- members and use policy are removed, and nothing new can choose it. revision
-- starts at 1 and rises by one on every change to the pool, its members or its
-- use policy. Names are display labels, unique among live pools; identity is the id.
CREATE TABLE IF NOT EXISTS runner_pools (
    id           UUID PRIMARY KEY,
    name         TEXT NOT NULL CHECK (name <> '' AND char_length(name) <= 64),
    hosting_type TEXT NOT NULL CHECK (hosting_type IN ('remote_provided', 'self_hosted')),
    state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled', 'deleted')),
    revision     BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS runner_pools_live_name_idx ON runner_pools (lower(name)) WHERE state <> 'deleted';

-- One member is one runner (self_hosted pools: a claimed runner, added by its own
-- owner) or one configured executor (remote_provided pools), never both. The
-- hosting kind is checked by the writer, which holds the pool row.
CREATE TABLE IF NOT EXISTS runner_pool_members (
    pool_id     UUID NOT NULL REFERENCES runner_pools (id),
    runner_id   UUID REFERENCES runners (id) ON DELETE CASCADE,
    executor_id TEXT CHECK (executor_id IS NULL OR executor_id ~ '^[a-z0-9][a-z0-9_.-]{0,62}$'),
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((runner_id IS NULL) <> (executor_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS runner_pool_members_runner_idx ON runner_pool_members (pool_id, runner_id) WHERE runner_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS runner_pool_members_executor_idx ON runner_pool_members (pool_id, executor_id) WHERE executor_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS runner_pool_members_by_runner_idx ON runner_pool_members (runner_id) WHERE runner_id IS NOT NULL;

-- A narrowing use policy on one remote_provided pool: with none, everyone who may
-- launch remote runs may use the pool; with one, only the listed subjects. The
-- writer refuses an empty list, so a row never reads as "nobody".
CREATE TABLE IF NOT EXISTS runner_pool_use_policies (
    pool_id    UUID PRIMARY KEY REFERENCES runner_pools (id),
    subjects   JSONB NOT NULL CHECK (jsonb_typeof(subjects) = 'array' AND jsonb_array_length(subjects) > 0),
    revision   BIGINT NOT NULL CHECK (revision >= 1),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The organisation's defaults: one row. A default names a pool; it does not pin
-- one, so a pool that is deleted or switched off later is refused at use, never
-- replaced.
CREATE TABLE IF NOT EXISTS runner_pool_org_defaults (
    singleton         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    preferred_hosting TEXT NOT NULL DEFAULT '' CHECK (preferred_hosting IN ('', 'remote_provided', 'self_hosted')),
    remote_provided   UUID REFERENCES runner_pools (id),
    self_hosted       UUID REFERENCES runner_pools (id),
    updated_by        TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
