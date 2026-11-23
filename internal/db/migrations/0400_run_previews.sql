-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_previews: one row for each preview URL a person opened on a port a run serves. A row is live until
-- revoked_at is set, and a run has at most one live row per port. visibility is who may open it: private (the
-- run's entry rule), org (people of this deployment) or public (anyone holding the link). A public row stores
-- only the SHA-256 of its link token, never the token, and the hash is unique. Every time is the database's:
-- created_at, updated_at and expires_at are written with now(), and a replica's clock decides nothing about
-- access. created_by and revoked_by hold principals, never emails.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds DELETE on run_attach_leases gets SELECT, INSERT, UPDATE and DELETE on run_previews.
CREATE TABLE IF NOT EXISTS run_previews (
    id          UUID        PRIMARY KEY,
    run_id      UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    port        INT         NOT NULL CHECK (port BETWEEN 1024 AND 65535),
    visibility  TEXT        NOT NULL CHECK (visibility IN ('private', 'org', 'public')),
    link_hash   BYTEA       CHECK ((visibility = 'public') = (link_hash IS NOT NULL)),
    created_by  TEXT        NOT NULL CHECK (created_by <> ''),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    revoked_by  TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS run_previews_one_live ON run_previews (run_id, port) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS run_previews_link ON run_previews (link_hash) WHERE link_hash IS NOT NULL;

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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_previews TO %I', r.rolname);
    END LOOP;
END
$mig$;
