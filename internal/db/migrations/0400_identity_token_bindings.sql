-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- identity_token_bindings: the delegation chain of each run token the spire identity provider mints, keyed by
-- the token's jti (base64url of the token's SHA-256). A SPIRE JWT-SVID carries only sub, aud and exp, so the
-- subject, sponsor and operator_owned the API needs back at verify live here, beside the run and the SPIFFE ID
-- the token was signed for. The key is the token, not the run: a revive recomputes the sponsor, so one row per
-- run would refuse it. A row is useless once its token is past expires_at; every mint and revoke deletes, for
-- any run, up to 1000 rows past expires_at plus the run token's lapse allowance. Erasing a person deletes the
-- rows whose sub or sponsor is that person, compared lower-cased, which the second index serves. Never a secret.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds DELETE on run_attach_leases gets SELECT, INSERT and DELETE on identity_token_bindings.
CREATE TABLE IF NOT EXISTS identity_token_bindings (
    jti            TEXT        PRIMARY KEY,
    run_id         UUID        NOT NULL,
    spiffe_id      TEXT        NOT NULL,
    sub            TEXT        NOT NULL,
    sponsor        TEXT        NOT NULL,
    operator_owned BOOLEAN     NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS identity_token_bindings_expires_at ON identity_token_bindings (expires_at);
CREATE INDEX IF NOT EXISTS identity_token_bindings_people ON identity_token_bindings (lower(sub), lower(sponsor));

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
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON identity_token_bindings TO %I', r.rolname);
    END LOOP;
END
$mig$;
