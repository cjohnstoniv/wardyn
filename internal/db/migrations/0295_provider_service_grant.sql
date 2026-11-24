-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- provider_service_grant_status: what each server replica last saw when it asked a provider's identity
-- service for a service identity's token, one row per (provider, replica). attempted_at orders one replica's
-- own outcomes (written on that replica's clock); seen_at is the database's time of the replica's last
-- refresher pass, which is what says the replica is alive. A row not seen for 24 hours is deleted by the
-- refresher. Nothing here is a secret or a token: a closed reason, the identity service's error code and the
-- HTTP status only.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on delegates, which the application writes (store_delegates.go), gets SELECT, INSERT,
-- UPDATE and DELETE on provider_service_grant_status.
CREATE TABLE IF NOT EXISTS provider_service_grant_status (
    provider_uid  TEXT        NOT NULL,
    replica_id    TEXT        NOT NULL,
    replica_host  TEXT        NOT NULL,
    grant_digest  TEXT        NOT NULL,
    client_id     TEXT        NOT NULL,
    outcome       TEXT        NOT NULL CHECK (outcome IN ('ok', 'failed')),
    reason        TEXT        NOT NULL DEFAULT '',
    oauth_error   TEXT        NOT NULL DEFAULT '',
    http_status   INTEGER,
    attempted_at  TIMESTAMPTZ NOT NULL,
    last_ok_at    TIMESTAMPTZ,
    valid_until   TIMESTAMPTZ,
    egress        TEXT        NOT NULL DEFAULT '',
    seen_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider_uid, replica_id)
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
         WHERE c.oid = 'delegates'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON provider_service_grant_status TO %I', r.rolname);
    END LOOP;
END
$mig$;
