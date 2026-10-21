-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A drive's second place (#112, #113): the same share, as one person's own runner
-- sees it. A run on that runner mounts the drive from alt_host_root; a run on the
-- organisation's runners mounts it from the first place, as before. All three
-- columns are NULL on a drive with one place, which behaves exactly as it did.
--
-- Only a share can have a second place (a managed volume lives where Wardyn made
-- it), and the second place is a host_path on a runner. Written as equalities,
-- not an IN-list, so the closed-enum guards keep reading 0054's sets.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS alt_backend TEXT;
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS alt_host_root TEXT;
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS alt_runner_id UUID REFERENCES runners(id);
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_second_place_shape;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_second_place_shape CHECK (
    (alt_backend IS NULL AND alt_host_root IS NULL AND alt_runner_id IS NULL)
    OR (alt_backend = 'host_path' AND COALESCE(alt_host_root, '') <> '' AND alt_runner_id IS NOT NULL AND owner_subject = ''
        AND (backend = 'host_path' OR backend = 'k8s_pvc_static')));
