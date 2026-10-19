-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's local folder (types.RunLocalFolder): a folder on the person's computer that `wardyn sync`
-- seeds into the sandbox before the agent starts (run_local_folder.go). local_folder is written once,
-- with the run row; seeded_at or seed_error is written once, by the gateway, when the first sync of
-- that folder reports in, and dispatch holds the agent until one of them is set. NULL on every other run.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS local_folder JSONB,
    ADD COLUMN IF NOT EXISTS local_folder_seeded_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS local_folder_seed_error TEXT;
