-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- What Wardyn knows of the passes a person's folder-only runner made for a run's computer folder
-- (types.RunLocalFolderStatus, run_computer_folder.go): the first sync in and the one that brought
-- the sandbox's changes back, as the runner counted them. NULL until the first of them ends, and on
-- every run whose folder `wardyn sync` seeds instead.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS local_folder_status JSONB;
