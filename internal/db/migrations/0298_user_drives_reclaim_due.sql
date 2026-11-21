-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A kept drive whose run never started and is owed its reclaim: the time its next attempt is due, written in
-- the same statement that takes the run out of the queue, so a process that stops right after the transition
-- still leaves the work for a sweep. NULL on every other drive and once the drive is reclaimed or its owner's
-- other run is found to hold it.
--
-- Split-role installs grant the app role no extra privilege here: this is a column on a table it already
-- writes.
ALTER TABLE user_drives
    ADD COLUMN IF NOT EXISTS reclaim_due_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS user_drives_reclaim_due_idx ON user_drives (reclaim_due_at)
    WHERE reclaim_due_at IS NOT NULL;
