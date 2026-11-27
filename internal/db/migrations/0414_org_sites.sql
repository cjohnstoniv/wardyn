-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Organisation sites (0.11): an admin's label for a place (an office, an edge cluster, a cloud region) over
-- organization runner pools and admin drives. A site grants nothing: it only lets a run whose drives are homed
-- there prefer the site's default pool, and lets the run and its audit row name the site. A runner never
-- claims a site; an admin assigns pools to one.
--
-- org_sites: names are display labels, unique among live sites; identity is the id. A deleted site is a
-- tombstone (state 'deleted'), so a run placed at it keeps its record; it is deleted only while no pool and no
-- drive is assigned to it, and it keeps no default pool. default_pool_id, when set, is one of the site's own
-- pools, which the writer checks under the site's row lock. storage_label and storage_url are a note for
-- people: Wardyn never connects to the URL and stores no credential for it. revision starts at 1 and rises
-- by one on every change to the row. No column names a person: the audit row holds who wrote the site.
--
-- org_site_pools: a pool is at one site at most, and only an organization pool can be; assigning or removing
-- one raises the pool's own revision in the same transaction, so a run admitted before the move fails the run
-- insert's pool fence. org_site_drives: an admin drive's home site; a member drive is made where its run
-- goes, so it has no home. A drive's row goes with the drive.
--
-- The app role is granted the tables here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on runner_pools gets SELECT, INSERT, UPDATE and DELETE on the three tables.
CREATE TABLE IF NOT EXISTS org_sites (
    id              UUID        PRIMARY KEY,
    name            TEXT        NOT NULL CHECK (name <> '' AND char_length(name) <= 64),
    state           TEXT        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'deleted')),
    default_pool_id UUID        REFERENCES runner_pools (id),
    storage_label   TEXT        NOT NULL DEFAULT '' CHECK (char_length(storage_label) <= 128),
    storage_url     TEXT        NOT NULL DEFAULT '' CHECK (char_length(storage_url) <= 2048),
    revision        BIGINT      NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT org_sites_deleted_no_default CHECK (state = 'active' OR default_pool_id IS NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS org_sites_live_name_idx ON org_sites (lower(name)) WHERE state <> 'deleted';

CREATE TABLE IF NOT EXISTS org_site_pools (
    pool_id     UUID        PRIMARY KEY REFERENCES runner_pools (id),
    site_id     UUID        NOT NULL REFERENCES org_sites (id),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS org_site_pools_site_idx ON org_site_pools (site_id);

CREATE TABLE IF NOT EXISTS org_site_drives (
    drive_id    UUID        PRIMARY KEY REFERENCES user_drives (id) ON DELETE CASCADE,
    site_id     UUID        NOT NULL REFERENCES org_sites (id),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS org_site_drives_site_idx ON org_site_drives (site_id);

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'runner_pools'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON org_sites, org_site_pools, org_site_drives TO %I', r.rolname);
    END LOOP;
END
$mig$;
