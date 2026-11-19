-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A kept run whose end is held off by its own lock for too long is ended without that lock (a forced stop).
-- forced_stop_at is set in the same statement that makes the run terminal, and cleared once that stop has
-- confirmed the run's sandbox gone or kept with only its proxy stopped; while it is set, the watcher-cadence
-- retry stops the sandbox again. Nullable with no default, so the column is metadata-only.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS forced_stop_at TIMESTAMPTZ;

-- The retry runs every minute on every replica and finds the marked runs through this partial index, so its
-- cost follows the few marked runs and not the stored ones (as 0215's does).
CREATE INDEX IF NOT EXISTS agent_runs_forced_stop_idx
    ON agent_runs (id) WHERE forced_stop_at IS NOT NULL;
