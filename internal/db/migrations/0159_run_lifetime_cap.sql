-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The latest end a run's pool and the deployment allow, measured from launch
-- (created_at plus the lowest of their maximum lifetimes), and which of the two
-- bound it. An extension is bounded by this absolute time as well as by the
-- governance lease, so a renewable "from now" limit cannot carry a run past the
-- maximum lifetime it launched under.
--
-- NULL for every existing row and for a run no pool or deployment maximum
-- bounds: no absolute cap, the behaviour every run had before. The source is ''
-- exactly when the cap is NULL.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS lifetime_cap_at timestamptz,
    ADD COLUMN IF NOT EXISTS lifetime_cap_source TEXT NOT NULL DEFAULT '' CHECK (lifetime_cap_source IN ('', 'pool', 'deployment'));
