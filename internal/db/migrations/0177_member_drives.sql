-- Member drives: a person's own drive is a user_drives row that only they own.
--
-- owner_subject is the owner's canonical subject, '' on every admin row. It is
-- written once, by store.CreateMemberDrive, and no statement updates it.
-- delete_started_at is a member row's delete tombstone: set before the storage
-- object is destroyed, and the row is removed only after.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS owner_subject     TEXT NOT NULL DEFAULT '';
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS delete_started_at TIMESTAMPTZ;

-- Admin rows keep 0054's global name rule and 0061's global slug rule. Member
-- rows are unique per owner, so no person can squat a name on another, and the
-- per-owner index also serves the per-person count.
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS user_drives_admin_name_uniq ON user_drives (name) WHERE owner_subject = '';
DROP INDEX IF EXISTS user_drives_name_slug_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS user_drives_name_slug_uniq ON user_drives (name_slug)
    WHERE backend <> 'host_path' AND name_slug <> '' AND owner_subject = '';
CREATE UNIQUE INDEX IF NOT EXISTS user_drives_member_slug_uniq ON user_drives (owner_subject, name_slug)
    WHERE owner_subject <> '';

-- A member row can never name a host path, a pre-provisioned claim, a
-- claim-derived home or a slug-scheme object. Written as equalities, not an
-- IN-list, so the closed-enum guards keep reading 0054's sets for each column.
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_member_managed;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_member_managed CHECK (owner_subject = '' OR
    ((backend = 'docker_volume' OR backend = 'k8s_pvc') AND host_root = '' AND home_template = 'hash'
     AND object_scheme = 'id'));
ALTER TABLE user_drives DROP CONSTRAINT IF EXISTS user_drives_tombstone_member_only;
ALTER TABLE user_drives ADD CONSTRAINT user_drives_tombstone_member_only
    CHECK (delete_started_at IS NULL OR owner_subject <> '');
