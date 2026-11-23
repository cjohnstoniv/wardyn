-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A person's own team drive (#3459): a member drive (owner_subject set) whose one folder is shared with everyone in
-- the governance unit it belongs to and the units below it.
--
-- team_unit_id is the unit the person was in when they made it. A deleted unit sets it NULL, which leaves the drive
-- to its owner alone. team_members_can_write is a super admin's tick that lets the unit write; FALSE by default, and
-- set only on such a row. Written as equalities, not an IN-list, so the closed-enum guards keep reading 0054's sets.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS team_unit_id UUID REFERENCES governance_units (id) ON DELETE SET NULL;
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS team_members_can_write BOOLEAN NOT NULL DEFAULT FALSE;

-- 0205's shape, plus a person's own team drive, which is a Docker volume only: a k8s_pvc claim mounts in one pod at a
-- time, and a Mac disk in one VM.
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_team_shape;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_team_shape CHECK (NOT team OR
    (owner_subject = '' AND (backend = 'host_path' OR backend = 'k8s_pvc_static' OR backend = 'docker_volume')) OR
    (owner_subject <> '' AND backend = 'docker_volume'));

ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_team_unit_shape;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_team_unit_shape CHECK (
    (team_unit_id IS NULL AND NOT team_members_can_write) OR (team AND owner_subject <> ''));
