-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The extra containers a run keeps beside its agent, as create admitted them (one row per run that has any).
CREATE TABLE IF NOT EXISTS run_containers (
    run_id     uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    containers jsonb NOT NULL CHECK (jsonb_typeof(containers) = 'array' AND jsonb_array_length(containers) BETWEEN 1 AND 4)
);

-- Split-role installs: the app role writes this table at create, so every role that may update agent_runs gets the same.
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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_containers TO %I', r.rolname);
    END LOOP;
END
$mig$;
