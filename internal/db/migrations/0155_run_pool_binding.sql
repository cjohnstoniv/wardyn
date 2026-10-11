-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The runner pool a run was admitted into (0.9 runner pools), written once in the
-- run's own INSERT and never re-derived. A pool is a label for a choice and never
-- admission: the run is admitted against the exact target recorded here (runner_id
-- for a self-hosted pool, runner_pool_executor for a remote-provided one), and
-- dispatch routes to that target whatever the pool, its members or anyone's
-- defaults say later.
--
--   runner_pool_id         the pool; NULL for a run from before pools and for a
--                          server-originated run (scans, record, verify, login) that
--                          names none. A NULL pool is distinguishable from a pool
--                          bound to the run: the selection below is then empty.
--   runner_pool_revision   the pool revision the run was admitted against.
--   runner_pool_selection  how the pool was chosen: explicit, personal_default or
--                          organisation_default.
--   runner_pool_executor   the configured executor of a remote-provided pool the run
--                          was bound to; empty for a self-hosted pool (its target is
--                          runner_id, 0141) and for an unpooled run.
--
-- The foreign key keeps the default NO ACTION on purpose. DeleteRunnerPool leaves a
-- tombstone row (state 'deleted'), so a run's pool id keeps resolving and a deleted
-- pool's history stays readable; a hard delete of a pool that runs reference is
-- refused rather than silently orphaning the audit record.
--
-- The CHECK ties the pool id to its selection. CreateRun fences the pool revision
-- inside its own INSERT by reading runner_pool_id through the pool row, so a pool
-- that moved or was switched off between admission and insert yields a NULL id with
-- a selection, which this constraint refuses (store.ErrRunnerPoolStale).
--
-- Split-role installs grant the app role no extra privilege here: these are columns
-- on a table it already writes.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS runner_pool_id        UUID REFERENCES runner_pools (id),
    ADD COLUMN IF NOT EXISTS runner_pool_revision  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS runner_pool_selection TEXT NOT NULL DEFAULT '' CHECK (runner_pool_selection IN ('', 'explicit', 'personal_default', 'organisation_default')),
    ADD COLUMN IF NOT EXISTS runner_pool_executor  TEXT NOT NULL DEFAULT '';

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_runner_pool_binding;
ALTER TABLE agent_runs
    ADD CONSTRAINT agent_runs_runner_pool_binding CHECK ((runner_pool_id IS NULL) = (runner_pool_selection = ''));

CREATE INDEX IF NOT EXISTS agent_runs_runner_pool_idx ON agent_runs (runner_pool_id) WHERE runner_pool_id IS NOT NULL;
