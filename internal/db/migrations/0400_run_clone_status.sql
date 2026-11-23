-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_clone_status: what a run's launcher said about each repository clone (#3159), one row per run, read once by
-- the server over the exec channel and deleted with the run. state is cloning (the launcher's in-progress marker
-- was seen: the run reached the clone step, and a final state replaces it once), reported (the file was read),
-- not_reported (the launcher started its clones and sent no outcome) or none (the run never cloned). repos holds
-- the entries: repository, folder, outcome and cause. The file is the sandbox's own word, a hint and never a verdict.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner that
-- holds UPDATE on agent_runs, which the application writes, gets SELECT, INSERT and DELETE on run_clone_status.
-- The table alters nothing existing.
CREATE TABLE IF NOT EXISTS run_clone_status (
    run_id      UUID        PRIMARY KEY REFERENCES agent_runs (id) ON DELETE CASCADE,
    state       TEXT        NOT NULL CHECK (state IN ('cloning', 'reported', 'not_reported', 'none')),
    repos       JSONB       NOT NULL DEFAULT '[]',
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
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
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON run_clone_status TO %I', r.rolname);
    END LOOP;
END
$mig$;
