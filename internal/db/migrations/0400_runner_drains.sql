-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A drain an administrator set on a registered runner, one row per drained runner (types.RunnerDrain):
--   created_by  the administrator's principal
--   created_at  when the drain was set
--   reason      why, as written (the handler bounds it to 512 characters without control characters)
-- A drained runner takes no new sandbox: placement, the queue's start, a lost run's revive and a kept machine's
-- resume pass it over. Its running runs are not touched. Undrain deletes the row. A revoke leaves the row, inert:
-- a revoked runner takes nothing, and its reason stays here in clear. ON DELETE CASCADE covers the delete of an
-- unclaimed runner only.
--
-- ADDITIVE ONLY: a new table. A downgrade stops reading it, and every drained runner takes runs again.
CREATE TABLE IF NOT EXISTS runner_drains (
    runner_id  UUID PRIMARY KEY REFERENCES runners (id) ON DELETE CASCADE,
    created_by TEXT NOT NULL CHECK (created_by <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2048)
);

-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on runners gets SELECT, INSERT and DELETE on runner_drains.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'runners'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON runner_drains TO %I', r.rolname);
    END LOOP;
END
$mig$;
