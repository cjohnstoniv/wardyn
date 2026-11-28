-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's site (0.11). agent_runs.site_id is the organisation site of the pool a run was placed in, written
-- once at create while sites are on; NULL for every run before this migration, for a run with no pool, and
-- for a pool at no site. A site is never removed, only marked deleted, so the reference outlives it.
-- runner_pool_selection admits site_default beside the three it had: the run had no pool named, and its
-- drives are homed at a site whose default pool was chosen ahead of the person's and the organisation's.
--
-- WIDENS one CHECK and changes no row. Re-runnable: the constraint is dropped if present first. Split-role
-- installs grant the app role no extra privilege here: a new column on a table it already writes.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS site_id UUID REFERENCES org_sites (id);
ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_runner_pool_selection_check,
    ADD CONSTRAINT agent_runs_runner_pool_selection_check
        CHECK (runner_pool_selection IN ('', 'explicit', 'personal_default', 'organisation_default', 'site_default'));
