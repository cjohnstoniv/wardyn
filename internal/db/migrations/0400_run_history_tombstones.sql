-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One row per run the run-history retention sweep deleted (#4068, WARDYN_RUN_HISTORY_RETENTION_DAYS).
-- A deleted run's audit rows stay: audit_events.run_id has no foreign key. This row keeps what still reads
-- those rows by run: the unit a unit admin's audit view selects by, and the run id a device's federated
-- audit rows may not claim. created_by is the person who created the run: erasing that person finds the
-- run's recordings through it (a recording outlives its run), then blanks it. Nothing else updates a row,
-- and nothing deletes one.
--
-- The app role is granted the table here, found from the catalog and never by name, as 0295 does: every role but
-- the owner that holds UPDATE on agent_runs, which the application writes, gets SELECT, INSERT and UPDATE on
-- run_history_tombstones, so the unit admins' audit view, the device audit ingest and person erasure use it
-- from the upgrade on.
-- DELETE on agent_runs is the operator's grant before turning the sweep on (CHANGELOG, Before you upgrade).
CREATE TABLE IF NOT EXISTS run_history_tombstones (
    run_id     UUID        PRIMARY KEY,
    unit_id    UUID,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS run_history_tombstones_unit_idx
    ON run_history_tombstones (unit_id) WHERE unit_id IS NOT NULL;

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'agent_runs'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON run_history_tombstones TO %I', r.rolname);
    END LOOP;
END
$mig$;
