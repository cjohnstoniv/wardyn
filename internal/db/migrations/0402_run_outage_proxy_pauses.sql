-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Whether the run's active proxy pauses at its allowance and asks to recover (#4082). Written per proxy start.
ALTER TABLE run_outage_terms ADD COLUMN proxy_pauses boolean NOT NULL DEFAULT false;

-- The store writes this one column after a row exists (a proxy start clears it, a proxy's loaded report sets it), so
-- the roles granted the table in 0401 get UPDATE on proxy_pauses alone, found from the catalog the same way.
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
        EXECUTE format('GRANT UPDATE (proxy_pauses) ON run_outage_terms TO %I', r.rolname);
    END LOOP;
END
$mig$;
