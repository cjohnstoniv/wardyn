-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The extra containers a run keeps beside its agent, as create admitted them (one row per run that has any).
CREATE TABLE IF NOT EXISTS run_containers (
    run_id     uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    containers jsonb NOT NULL CHECK (jsonb_typeof(containers) = 'array' AND jsonb_array_length(containers) BETWEEN 1 AND 4)
);
