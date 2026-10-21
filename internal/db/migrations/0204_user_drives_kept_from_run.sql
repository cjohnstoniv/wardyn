-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run a member drive was kept from (#1991, keep_work_folder): written once, by
-- store.CreateMemberDrive, when a run keeps its work folder as its person's drive. NULL on every
-- drive a person or an admin made by hand, and on every drive created before this column. No
-- foreign key: the drive outlives the run, and a run's row going does not touch the drive.
--
-- Split-role installs grant the app role no extra privilege here: this is a column on a table it
-- already writes.
ALTER TABLE user_drives
    ADD COLUMN IF NOT EXISTS kept_from_run UUID;
