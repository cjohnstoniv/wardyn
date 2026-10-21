-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Team drives (#1988): an admin drive whose allocation gives a group ONE shared
-- folder instead of a folder per person. Honoured only while the site switch
-- storage.user_drive.team_drives is on; with it off a team row resolves per person.
--
-- team is admin-only and never on a k8s_pvc drive (ReadWriteOnce, one pod per
-- claim). Written as equalities, not an IN-list, so the closed-enum guards keep
-- reading 0054's sets for each column.
--
-- members_can_write is the allocation's write bit on a team drive. FALSE by
-- default: a team allocation is read-only unless the admin ticks it.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS team BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_team_shape;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_team_shape CHECK (NOT team OR (owner_subject = '' AND
    (backend = 'host_path' OR backend = 'k8s_pvc_static' OR backend = 'docker_volume')));

ALTER TABLE user_drive_grants ADD COLUMN IF NOT EXISTS members_can_write BOOLEAN NOT NULL DEFAULT FALSE;
