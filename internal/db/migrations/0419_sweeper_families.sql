-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The sweep families' lease epochs. The sweeps that must run once are split into families, each led by one replica
-- at a time under a session-level advisory lock of its own (db.SweeperFamilyLockClass), which a Postgres failover can
-- release under a still-running leader. Each acquisition bumps its family's epoch, and a multi-step operation re-reads
-- it before it writes: the older leader sees a newer epoch and stops. One row per family (db.SweeperFamilies), seeded
-- here. Expand-only: sweeper_leader (0110) is kept, and every family acquisition still bumps it for older replicas.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on sweeper_leader, which the election writes, gets SELECT and UPDATE on sweeper_families.
CREATE TABLE IF NOT EXISTS sweeper_families (
    family      text PRIMARY KEY,
    epoch       bigint NOT NULL DEFAULT 0,
    holder      text NOT NULL DEFAULT '',
    acquired_at timestamptz
);

INSERT INTO sweeper_families (family) VALUES ('runs'), ('credentials'), ('audit'), ('records')
    ON CONFLICT (family) DO NOTHING;

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'sweeper_leader'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, UPDATE ON sweeper_families TO %I', r.rolname);
    END LOOP;
END
$mig$;
