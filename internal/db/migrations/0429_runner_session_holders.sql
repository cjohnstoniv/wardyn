-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Which replica holds each registered runner's session, so that a call on another replica reaches the runner
-- through that one. The holding replica claims its row only while runner_inventories still names the same session,
-- refreshes seen_at every 10 seconds while the link is up, and deletes the row when the session ends. A row older
-- than 30 seconds is never routed to. runner_peer_replicas maps each server process's watcher lease owner to its
-- replica and internal address, so a runner's output can reach the process that holds the run's watcher lease. Each
-- process upserts its own row; rows unseen for a day are deleted. Expand-only: two new tables, nothing changed.
--
-- The app role is granted both tables here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on runner_inventories, which the runner hub writes, gets SELECT, INSERT, UPDATE and DELETE.
CREATE TABLE IF NOT EXISTS runner_session_holders (
    runner_id  uuid PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
    session_id uuid NOT NULL,
    replica_id text NOT NULL,
    peer_url   text NOT NULL,
    seen_at    timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS runner_peer_replicas (
    lease_owner text PRIMARY KEY,
    replica_id  text NOT NULL,
    peer_url    text NOT NULL,
    seen_at     timestamptz NOT NULL
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
         WHERE c.oid = 'runner_inventories'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON runner_session_holders, runner_peer_replicas TO %I', r.rolname);
    END LOOP;
END
$mig$;
