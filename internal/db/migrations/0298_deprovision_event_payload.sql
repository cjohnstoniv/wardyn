-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- deprovision_jobs.audit_event_payload: the payload of the audit event a portal person's suspension or purge step
-- writes, pinned beside the event's id and time (0196) by the first attempt, so a completion row that one replica
-- spooled and lost is written again by another with the same identity, actor and data. A human actor's payload is
-- stored sealed under that person's audit key, so erasing the key erases it; the step's finish clears it in the same
-- statement that marks the step done, once the event is in the audit table. Reopening a step clears it with the id and
-- time.
-- deprovision_jobs.kill_claimed_at: when a worker claimed a portal suspension's kill of one run. A second claim of
-- the same occurrence waits out the claim's lease instead of tearing the run down again.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS audit_event_payload JSONB;
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_claimed_at TIMESTAMPTZ;
-- deprovision_jobs.kill_claim_id and kill_from: the acquisition that holds the kill claim, so only its holder releases
-- or finishes the row, and the state the run was killed from, so a repair of a run that left the queue is a queued
-- kill's repair.
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_claim_id UUID;
ALTER TABLE deprovision_jobs ADD COLUMN IF NOT EXISTS kill_from TEXT;

-- principal_identities.first_login_at (0296) is the instant last_login_at first holds a value: the instant of the
-- first sign-in through the identity, whichever replica wrote it. A 0.10.1 replica, which keeps writing during a rolling
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
