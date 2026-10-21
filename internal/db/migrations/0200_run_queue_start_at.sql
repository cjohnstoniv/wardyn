-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A scheduled run's not-before time (one-time scheduled runs, behind WARDYN_SCHEDULED_RUNS). NULL for
-- a run that waits from its create, so every existing row is unchanged. queued_at stays the create
-- time, the issue time of the authority the row holds; promotion never judges a row before start_at,
-- and a row stands in its pool's line at COALESCE(start_at, queued_at).
--
-- Split-role installs grant the app role no extra privilege here: this is a column on a table it
-- already writes.
ALTER TABLE run_queue
    ADD COLUMN IF NOT EXISTS start_at TIMESTAMPTZ;
