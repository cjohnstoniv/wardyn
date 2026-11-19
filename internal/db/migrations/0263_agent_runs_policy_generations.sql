-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The policies a run's proxy has enforced, one entry per generation: dispatch's (generation 1) and one per
-- revive, each with its digest and the ids of the run tokens its proxy held, so a row the proxy sent is
-- stamped with the generation of the token it presented. '[]' is a run never revived, which is every
-- existing row: its one policy is policy_digest. agent_runs already has its grants.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS policy_generations JSONB NOT NULL DEFAULT '[]'::jsonb;
