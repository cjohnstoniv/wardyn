-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's outage terms (#4082), fixed at dispatch and written once.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner that
-- holds UPDATE on agent_runs, which dispatch writes on the same connection, gets SELECT and INSERT on
-- run_outage_terms. The store only inserts a row (ON CONFLICT DO NOTHING, which needs no UPDATE) and reads it; a run's
-- row goes with the run through ON DELETE CASCADE, which runs as the table's owner, so no UPDATE or DELETE is granted.
CREATE TABLE run_outage_terms (
    run_id         uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    allowance_s    integer NOT NULL CHECK (allowance_s BETWEEN 1800 AND 86400),
    strict         boolean NOT NULL,
    end_unattended boolean NOT NULL,
    written_at     timestamptz NOT NULL DEFAULT now()
);

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
        EXECUTE format('GRANT SELECT, INSERT ON run_outage_terms TO %I', r.rolname);
    END LOOP;
END
$mig$;
