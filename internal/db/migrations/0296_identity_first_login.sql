-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- principal_identities.first_login_at: when a person first signed in through this identity, kept apart from
-- when the identity row was made. A SCIM user is provisioned as a row with no principal, often long before
-- their first sign-in binds it, so created_at is the provisioning time and is no evidence of a sign-in.
-- The sign-in path writes it once and never changes it.
--
-- A row made by a sign-in has created_at as its first sign-in, which the backfill below records. A row any
-- SCIM write has touched is left NULL: its first sign-in cannot be told from its provisioning, and the people
-- directory shows an unknown date for it, not a wrong one. A portal person's first use is their row's
-- created_at, which the directory reads directly. The column holds a time and no secret.
ALTER TABLE principal_identities ADD COLUMN IF NOT EXISTS first_login_at TIMESTAMPTZ;

-- The trigger is made here, with the column and before the backfill, in this migration's one transaction: the column
-- is never visible without it, so no write of a 0.10.1 replica can land between the two. first_login_at is the
-- instant last_login_at first holds a value: the instant of the first sign-in through the identity, whichever replica wrote it. A 0.10.1 replica, which keeps writing during a rolling
-- upgrade after every migration has applied, inserts a sign-in's row with last_login_at set and no first_login_at,
-- and stamps last_login_at on a row a SCIM write made, without it; the sign-in path of this release reads such a
-- row's last_login_at as proof the first sign-in is past and leaves first_login_at NULL for good. The trigger
-- gives those writes the value, from the same fact the release's own statements use. A SCIM write that sets no
-- last_login_at leaves it NULL, so provisioning is still no sign-in, and a statement that names first_login_at is
-- not changed. A column DEFAULT could not tell a sign-in's row from a SCIM write's, and a read rule could not once a
-- SCIM write has touched the row.
CREATE OR REPLACE FUNCTION principal_identities_first_login() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.first_login_at IS NULL AND NEW.last_login_at IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.last_login_at IS NULL) THEN
        NEW.first_login_at := NEW.last_login_at;
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS principal_identities_first_login ON principal_identities;
CREATE TRIGGER principal_identities_first_login BEFORE INSERT OR UPDATE OF last_login_at ON principal_identities
    FOR EACH ROW EXECUTE FUNCTION principal_identities_first_login();

UPDATE principal_identities
   SET first_login_at = created_at
 WHERE first_login_at IS NULL
   AND last_login_at IS NOT NULL
   AND scim_external_id = ''
   AND scim_user_name = ''
   AND NOT starts_with(issuer, 'delegate:person:');
