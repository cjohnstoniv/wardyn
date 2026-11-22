-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One-way deletion of a person's drive.
--
-- delete_issues counts the physical deletes issued for a drive. It only grows, and goes back to 0 only with the
-- row's final DELETE. An odd value says a call to the storage may still be outstanding, whatever delete_started_at
-- says; a claim (delete_started_at with its delete_issues) is consumed by the first issue, so one claim makes at most
-- one call. The backfill treats a row still marked as issued: the old daemon cleared its mark on an ambiguous error,
-- so a row it left marked is one whose delete was meant and may be under way.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS delete_issues INTEGER NOT NULL DEFAULT 0;
UPDATE user_drives SET delete_issues = 1 WHERE delete_started_at IS NOT NULL AND delete_issues = 0;

-- legacy_unsettled marks a person's drive that existed when this migration ran, on a backend whose storage an old
-- daemon may still be deleting (a Mac disk, a Docker volume). A marked drive is listed but not attachable until
-- its Mac's runner proves it is a new process, or an administrator releases the Docker ones. Membership, not time:
-- a row inserted later keeps false whatever its created_at says.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS legacy_unsettled BOOLEAN NOT NULL DEFAULT false;
UPDATE user_drives SET legacy_unsettled = true
 WHERE owner_subject <> '' AND backend IN ('mac_disk', 'docker_volume') AND NOT legacy_unsettled;

-- drive_legacy_releases: the durable record of one release of the legacy Docker drives, committed with the release
-- itself and replayed to the audit log until the audit table holds it (audited_at then set, actor cleared).
-- id is the audit event's id, event_at its time (fixed to the microsecond before the insert).
CREATE TABLE IF NOT EXISTS drive_legacy_releases (
    id          UUID        PRIMARY KEY,
    event_at    TIMESTAMPTZ NOT NULL,
    actor_type  TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    released    INTEGER     NOT NULL CHECK (released > 0),
    audited_at  TIMESTAMPTZ
);

-- The app role is granted the table here, found from the catalog and never by name. The release statement
-- updates user_drives, so the roles that write it are the roles that hold UPDATE on user_drives (the table this
-- feature writes), not on delegates as 0295's anchor does: every such role but the owner gets SELECT, INSERT and
-- UPDATE (no DELETE: a record is settled, never removed) on drive_legacy_releases.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'user_drives'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON drive_legacy_releases TO %I', r.rolname);
    END LOOP;
END
$mig$;
