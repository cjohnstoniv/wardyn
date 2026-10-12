-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-drive grants (0.9): one subject may be granted several drives, one row per
-- drive. 0054 keyed user_drive_grants on (subject_type, subject), so a subject
-- held exactly one row and allocating another drive repointed it. A run may now
-- attach every drive its person is granted, each at its own path, so the natural
-- key gains the drive.
--
-- Existing rows are untouched: the old key was stricter, so every stored row is
-- already unique under the new one. The single-drive resolver
-- (store.PG.ResolveUserDrive) ranks every matching row and keeps one; it reads
-- the same rows in the same order, so the drive behind /home/agent/drive on an
-- upgraded database is the one it was before.
--
-- The new key leads with the same two columns, so it still serves the
-- resolver's subject lookup; no other index changes.

ALTER TABLE user_drive_grants DROP CONSTRAINT IF EXISTS user_drive_grants_subject_type_subject_key;
ALTER TABLE user_drive_grants DROP CONSTRAINT IF EXISTS user_drive_grants_subject_drive_key;
ALTER TABLE user_drive_grants
    ADD CONSTRAINT user_drive_grants_subject_drive_key UNIQUE (subject_type, subject, drive_id);
