-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- runner_event_refusal_windows: each runner's current window of runner.event.refuse rows, one row per runner,
-- so the bound on those rows holds across every replica and a restart. The refusal transaction that writes a
-- runner's refuse row locks the runner's row here first and counts in it: written is how many refuse rows the
-- window wrote (at most 33, the last marked suppressed) and suppressed how many refusals it left without a row,
-- written as one summary row by the runner's first refusal of a later window. window_start is the database's
-- time of the refusal that opened the window. A row goes with its runner; nothing else deletes or sweeps the
-- table, and the next window writes over a runner's row in place. Never a secret.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the
-- owner that holds UPDATE on runner_spool_cursors, which the same refusal transaction writes, gets SELECT,
-- INSERT and UPDATE on runner_event_refusal_windows.
CREATE TABLE IF NOT EXISTS runner_event_refusal_windows (
    runner_id    UUID        PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
    window_start TIMESTAMPTZ NOT NULL,
    written      INTEGER     NOT NULL DEFAULT 0 CHECK (written >= 0),
    suppressed   INTEGER     NOT NULL DEFAULT 0 CHECK (suppressed >= 0)
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
         WHERE c.oid = 'runner_spool_cursors'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON runner_event_refusal_windows TO %I', r.rolname);
    END LOOP;
END
$mig$;
