-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Windows VM licence pools. windows_licence_pools is each entitlement an organisation declares, with 1 to 10000
-- seats; a deleted pool stays as a tombstone. windows_licence_holders is who holds a seat in a pool, at most one
-- row per person per pool; subject is the person's subject, written lower-cased and trimmed. windows_images is
-- each Windows image the organisation registered, by a digest-pinned ref that is unique among active images, and
-- the pool its runs take a seat in; a retired image stays as a tombstone.
--
-- windows_licence_leases is the live seats only: one row for each windows_vm run that holds a seat, written and
-- removed by 0401's triggers on agent_runs when the run is inserted and when it ends. A holder has at most one
-- live seat per pool, and holders never exceed seats, so live seats never exceed seats. A lease's run cannot be
-- deleted (ON DELETE RESTRICT): only the run's end frees its seat.
--
-- The app role is granted the tables here, found from the catalog and never by name: every role but the owner
-- that holds DELETE on run_attach_leases gets SELECT, INSERT, UPDATE, DELETE on the pools, holders and images,
-- and SELECT only on the leases, so no app write takes or frees a seat. Never a secret.
CREATE TABLE IF NOT EXISTS windows_licence_pools (
    id         UUID        PRIMARY KEY,
    name       TEXT        NOT NULL,
    seats      INTEGER     NOT NULL CHECK (seats BETWEEN 1 AND 10000),
    state      TEXT        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'deleted')),
    revision   BIGINT      NOT NULL DEFAULT 1,
    created_by TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS windows_licence_pools_live_name ON windows_licence_pools (lower(name)) WHERE state = 'active';

CREATE TABLE IF NOT EXISTS windows_licence_holders (
    id       UUID        PRIMARY KEY,
    pool_id  UUID        NOT NULL REFERENCES windows_licence_pools (id),
    subject  TEXT        NOT NULL CHECK (subject <> '' AND subject = lower(btrim(subject))),
    added_by TEXT        NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS windows_licence_holders_one_per_person ON windows_licence_holders (pool_id, lower(subject));
CREATE INDEX IF NOT EXISTS windows_licence_holders_subject ON windows_licence_holders (lower(subject));

CREATE TABLE IF NOT EXISTS windows_images (
    id              UUID        PRIMARY KEY,
    name            TEXT        NOT NULL,
    ref             TEXT        NOT NULL CHECK (ref ~ '^[^@[:space:]]+@sha256:[0-9a-f]{64}$'),
    licence_pool_id UUID        NOT NULL REFERENCES windows_licence_pools (id),
    state           TEXT        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'retired')),
    revision        BIGINT      NOT NULL DEFAULT 1,
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS windows_images_live_ref ON windows_images (ref) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS windows_images_live_name ON windows_images (lower(name)) WHERE state = 'active';

CREATE TABLE IF NOT EXISTS windows_licence_leases (
    run_id    UUID        PRIMARY KEY REFERENCES agent_runs (id) ON DELETE RESTRICT,
    pool_id   UUID        NOT NULL REFERENCES windows_licence_pools (id),
    holder_id UUID        NOT NULL REFERENCES windows_licence_holders (id),
    image_id  UUID        NOT NULL REFERENCES windows_images (id),
    taken_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT windows_licence_leases_one_per_holder UNIQUE (pool_id, holder_id)
);
CREATE INDEX IF NOT EXISTS windows_licence_leases_holder ON windows_licence_leases (holder_id);
CREATE INDEX IF NOT EXISTS windows_licence_leases_image ON windows_licence_leases (image_id);

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'run_attach_leases'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON windows_licence_pools, windows_licence_holders, windows_images TO %I', r.rolname);
        EXECUTE format('GRANT SELECT ON windows_licence_leases TO %I', r.rolname);
    END LOOP;
END
$mig$;
