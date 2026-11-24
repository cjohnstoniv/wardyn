-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A person's disk drive (backend mac_disk) is in one run at a time: the Mac's own lock refuses a second VM when it
-- starts. mac_disk_holds lets create and Review refuse earlier: one row per drive, naming the run that was last
-- dispatched with it. A row counts only while that run is not finished, so nothing clears it (#3851).
CREATE TABLE IF NOT EXISTS mac_disk_holds (
    drive_id UUID PRIMARY KEY REFERENCES user_drives (id) ON DELETE CASCADE,
    run_id   UUID NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    held_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mac_disk_holds_run_id_idx ON mac_disk_holds (run_id);

-- The roles that write user_drives (every role but the owner holding UPDATE on it) write holds too, as 0299 grants
-- drive_legacy_releases: SELECT, INSERT and UPDATE. No DELETE: a hold goes only with its drive or its run.
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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON mac_disk_holds TO %I', r.rolname);
    END LOOP;
END
$mig$;
