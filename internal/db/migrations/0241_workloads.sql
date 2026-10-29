-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Workloads: a named, stored run request that Wardyn keeps running for its owner. One row per
-- (owner, name). The run it started is current_run_id; agent_runs gets no column, and the history of
-- which run came from which spec is the audit rows.
--
-- owner is a person, or service:<name> for a workload a custom resource wrote through a namespace
-- binding. source says which writer owns the row (the API and CLI, or a custom resource) and only that
-- writer may change it. For a custom resource, source_pool_id and source_namespace say where it lives,
-- source_runner_id is the runner that last reported it (its status goes back there), and k8s_generation
-- is the metadata.generation last recorded.
--
-- intent is the door-normalised intent of a `kind: Run` document. digest is the sha256 of the canonical
-- JSON of {intent, run_in, restart_key}; description is no part of it. generation counts digest changes
-- and current_generation is the one current_run_id was launched from, so a launch is owed while the
-- second is behind the first. last_refusal is {generation, reason, message, at} of the last refused
-- launch. launch_claim_until fences one replica to one launch; a claim a crash leaves lapses by itself.
-- launch_claim_token names the claim: only its holder records the launch's run or refusal, so a claimer
-- whose claim lapsed and was taken again cannot clear or overwrite the newer claimer's outcome.
-- status_pushed is the hash of the last status sent to the runner.
--
-- source_pool_id and current_run_id are cleared, not the row removed, if the pool or run row they name
-- is ever deleted. (A deleted pool normally stays as a tombstone, which keeps source_pool_id.)
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on it (CHANGELOG, Before you
-- upgrade).
CREATE TABLE IF NOT EXISTS workloads (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner              text NOT NULL,
    name               text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    source             text NOT NULL CHECK (source IN ('api', 'k8s')),
    source_pool_id     uuid REFERENCES runner_pools (id) ON DELETE SET NULL,
    source_namespace   text,
    source_runner_id   uuid,
    k8s_generation     bigint,
    description        text NOT NULL DEFAULT '',
    run_in             text NOT NULL DEFAULT '',
    restart_key        text NOT NULL DEFAULT '',
    intent             jsonb NOT NULL,
    digest             text NOT NULL,
    generation         bigint NOT NULL DEFAULT 1 CHECK (generation >= 1),
    current_run_id     uuid REFERENCES agent_runs (id) ON DELETE SET NULL,
    current_generation bigint NOT NULL DEFAULT 0 CHECK (current_generation >= 0),
    last_refusal       jsonb CHECK (last_refusal IS NULL OR jsonb_typeof(last_refusal) = 'object'),
    launch_claim_until timestamptz,
    launch_claim_token uuid,
    status_pushed      text NOT NULL DEFAULT '',
    created_by         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner, name),
    CHECK (source = 'k8s' OR (source_pool_id IS NULL AND source_namespace IS NULL AND source_runner_id IS NULL AND k8s_generation IS NULL)),
    CHECK (source <> 'k8s' OR source_namespace IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS workloads_current_run_idx ON workloads (current_run_id) WHERE current_run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS workloads_source_pool_idx ON workloads (source_pool_id, source_namespace) WHERE source = 'k8s';

-- A namespace binding: the custom resources of one Kubernetes namespace, on the runners of one pool,
-- launch as a service identity. It is the only authority such a resource has. sponsor is the identity's
-- sponsor when the binding was made; a new sponsor, a suspended or retired identity, or the sponsor's
-- cut sessions end it as authority without removing the row. A pool's deletion removes its bindings.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on it (CHANGELOG, Before you
-- upgrade).
CREATE TABLE IF NOT EXISTS workload_bindings (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id    uuid NOT NULL REFERENCES runner_pools (id) ON DELETE CASCADE,
    namespace  text NOT NULL CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    service    text NOT NULL,
    sponsor    text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (pool_id, namespace)
);
CREATE INDEX IF NOT EXISTS workload_bindings_namespace_idx ON workload_bindings (namespace);
