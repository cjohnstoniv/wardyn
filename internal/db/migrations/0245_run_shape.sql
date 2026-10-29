-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The New Run shape a run was set up from (agent_task, environment or application), written once at
-- insert: the request's own, else the one shape its experience, workload and tools fit. '' is a run
-- whose request fits more than one or names none, which is every existing row. agent_runs already
-- has its grants.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS shape TEXT NOT NULL DEFAULT ''
        CHECK (shape IN ('', 'agent_task', 'environment', 'application'));
