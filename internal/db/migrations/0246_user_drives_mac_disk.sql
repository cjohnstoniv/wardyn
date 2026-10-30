-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A disk kept on one Mac: a person's own drive, or a kept work folder, that a macOS VM attaches as a
-- second disk. Its bytes are a disk image on that Mac, so home_runner_id names the Mac, and is set on
-- a mac_disk row and on no other. Only a person's own drive is one, under 0177's member rule.
--
-- FORWARD-ONLY. A downgrade with mac_disk rows present is unsupported: the old backend CHECK and
-- member rule would refuse them, and an older server cannot reach the disks they name.
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_backend_check;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_backend_check
    CHECK (backend IN ('docker_volume', 'host_path', 'k8s_pvc', 'k8s_pvc_static', 'mac_disk'));

-- 0177's member rule, widened by one equality. Written as equalities, not an IN-list, so the
-- closed-enum guards keep reading the backend set above.
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_member_managed;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_member_managed CHECK (owner_subject = '' OR
    ((backend = 'docker_volume' OR backend = 'k8s_pvc' OR backend = 'mac_disk') AND host_root = ''
     AND home_template = 'hash' AND object_scheme = 'id'));

ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS home_runner_id UUID REFERENCES runners(id);
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_mac_disk_home;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_mac_disk_home
    CHECK ((backend = 'mac_disk') = (home_runner_id IS NOT NULL));
