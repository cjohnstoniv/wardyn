-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- agent_rules: an organisation's rules for one agent harness (#2949), met into every run that includes the
-- harness. One row per harness; version counts the writes, so a write names the version it replaces.
CREATE TABLE IF NOT EXISTS agent_rules (
    harness     TEXT        PRIMARY KEY CHECK (harness <> '' AND harness <> 'none'),
    spec        JSONB       NOT NULL,
    inner_walls JSONB,
    version     INTEGER     NOT NULL CHECK (version > 0),
    updated_by  TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on delegates, which the application writes (store_delegates.go), gets SELECT, INSERT,
-- UPDATE and DELETE on agent_rules.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'delegates'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON agent_rules TO %I', r.rolname);
    END LOOP;
END
$mig$;
