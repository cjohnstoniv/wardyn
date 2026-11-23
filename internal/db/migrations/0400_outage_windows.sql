-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Recorded outage windows (#4082): when the control plane could not be reached, when a runner's link was down, and when
-- the server's own proxies stopped renewing as a group. Written only by the control plane, on the database's clock.
--
-- The app role is granted the tables here, found from the catalog and never by name: every role but the owner that
-- holds UPDATE on agent_runs, which the lapse sweep on the same connection writes, gets SELECT, INSERT and UPDATE on
-- cp_heartbeat (the beat locks its row FOR UPDATE and updates it) and SELECT, INSERT, UPDATE and DELETE on
-- outage_windows (windows are opened, closed, healed and pruned). The id is an identity column, so an insert needs
-- no sequence privilege.
CREATE TABLE IF NOT EXISTS cp_heartbeat (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    beat_at   timestamptz NOT NULL
);
INSERT INTO cp_heartbeat (singleton, beat_at) VALUES (true, now()) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS outage_windows (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('cp_outage', 'runner_link', 'server_outage')),
    runner_id  uuid,
    started_at timestamptz NOT NULL,
    ended_at   timestamptz,
    detail     jsonb NOT NULL DEFAULT '{}',
    CHECK ((kind = 'runner_link') = (runner_id IS NOT NULL)),
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK (kind <> 'cp_outage' OR ended_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS outage_windows_kind_runner_started ON outage_windows (kind, runner_id, started_at);
CREATE UNIQUE INDEX IF NOT EXISTS outage_windows_one_open ON outage_windows
    (kind, COALESCE(runner_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE ended_at IS NULL;

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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON cp_heartbeat TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON outage_windows TO %I', r.rolname);
    END LOOP;
END
$mig$;
