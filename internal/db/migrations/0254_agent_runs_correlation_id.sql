-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The caller's own reference for a run (POST /runs correlation_id), written once at insert and
-- stamped into the data of the run's audit rows on their way to the audit sinks. '' is a run whose
-- caller gave none, which is every existing row. agent_runs already has its grants.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '';
