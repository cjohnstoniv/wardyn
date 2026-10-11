-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Runner pool limits (0.9): the pool's own limits per run type, as one JSON
-- object (types.RunnerPoolLimits) replaced whole by an administrator. NULL means
-- the pool adds no bound of its own, so every pool that exists before this
-- migration stays NULL. The column is validated by the writer; the CHECK only
-- keeps it an object. A change to it raises the pool's revision like any other.
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS limits JSONB
    CHECK (limits IS NULL OR jsonb_typeof(limits) = 'object');
