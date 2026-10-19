-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The harness's own walls a run launched with (#2822): its sandbox and where its approvals
-- happen, as {"sandbox": ..., "approvals": ...}. Written at create, and again when a queued run
-- starts, from the same fold that chose them. NULL for a run with no catalog harness and for a
-- run created before this column.
--
-- Split-role installs grant the app role no extra privilege here: this is a column on a table it
-- already writes.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS inner_walls JSONB;
