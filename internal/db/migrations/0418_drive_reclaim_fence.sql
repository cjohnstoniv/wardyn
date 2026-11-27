-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The reclaim fence on one person's storage object inside an organisation's drive.
--
-- drive_reclaims: one row per object a super admin's reclaim has claimed, keyed by the substrate's own name for it,
-- so one person's reclaim fences that person's volume or claim and no one else's. While a row exists no run attaches
-- the object. issues counts the deletes issued under the row, as user_drives.delete_issues does for a person's own
-- drive (0299): an odd value says a call to the storage may still be outstanding. The row goes only when the storage
-- confirmed the object gone or refused before touching it, so a delete that timed out keeps the object fenced until a
-- later reclaim confirms it. No foreign key to user_drives: the object outlives the drive row, and a row a deleted
-- drive left fences only an object of the same name.
CREATE TABLE IF NOT EXISTS drive_reclaims (
    backend     TEXT        NOT NULL,
    object_name TEXT        NOT NULL,
    drive_id    UUID        NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    issues      INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (backend, object_name)
);

-- drive_object_holds: each run dispatched with an object a reclaim could destroy (a Docker volume or a provisioned
-- claim). A hold counts only while its run has not ended, so nothing clears it; it goes with its run.
CREATE TABLE IF NOT EXISTS drive_object_holds (
    backend     TEXT        NOT NULL,
    object_name TEXT        NOT NULL,
    run_id      UUID        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    held_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (backend, object_name, run_id)
);
CREATE INDEX IF NOT EXISTS drive_object_holds_run_id_idx ON drive_object_holds (run_id);

-- The roles that write user_drives (every role but the owner holding UPDATE on it) write both tables, as 0299 grants
-- drive_legacy_releases: SELECT, INSERT, UPDATE and DELETE on drive_reclaims (a settled claim is deleted), SELECT and
-- INSERT on drive_object_holds (a hold goes only with its run).
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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON drive_reclaims TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT ON drive_object_holds TO %I', r.rolname);
    END LOOP;
END
$mig$;
