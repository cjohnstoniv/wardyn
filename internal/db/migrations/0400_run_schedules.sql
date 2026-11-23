-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Repeating scheduled runs (#3458, behind WARDYN_SCHEDULED_RUNS): one row per schedule a person keeps. It holds
-- the rule (every, weekday, local_time, time_zone), the run request less its start fields, and the identity the
-- schedule was made or last resumed under (authority, taken at authority_at). The leader sweep books each
-- occurrence as a one-time scheduled run a short lead before next_at. last_run_id points at the run the last
-- booking made and has no foreign key: a run deleted later leaves it dangling, and the screen shows no link.
-- This migration alters no existing table.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on run_queue, which the application writes (store_run_queue.go), gets SELECT, INSERT,
-- UPDATE and DELETE on run_schedules.
CREATE TABLE IF NOT EXISTS run_schedules (
    id             UUID        PRIMARY KEY,
    owner          TEXT        NOT NULL CHECK (owner <> ''),
    name           TEXT        NOT NULL DEFAULT '',
    every          TEXT        NOT NULL CHECK (every IN ('daily', 'weekdays', 'weekly')),
    weekday        SMALLINT    CHECK (weekday BETWEEN 0 AND 6),
    local_time     TEXT        NOT NULL CHECK (local_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    time_zone      TEXT        NOT NULL CHECK (time_zone <> ''),
    request        JSONB       NOT NULL,
    authority      JSONB       NOT NULL,
    authority_at   TIMESTAMPTZ NOT NULL,
    next_at        TIMESTAMPTZ NOT NULL,
    paused_at      TIMESTAMPTZ,
    paused_reason  TEXT        NOT NULL DEFAULT '',
    last_due_at    TIMESTAMPTZ,
    last_run_id    UUID,
    last_outcome   TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((every = 'weekly') = (weekday IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS run_schedules_owner ON run_schedules (owner);
CREATE INDEX IF NOT EXISTS run_schedules_due ON run_schedules (next_at) WHERE paused_at IS NULL;

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'run_queue'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_schedules TO %I', r.rolname);
    END LOOP;
END
$mig$;
