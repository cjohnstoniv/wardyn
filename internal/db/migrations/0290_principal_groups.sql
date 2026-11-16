-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- principal_groups: what each person's last sign-in through one door carried, one row per person and
-- door. portal is '' for an SSO sign-in to Wardyn itself and a portal's id for a token exchange through
-- that portal. Each row holds the token's email as sent, the group snapshot (the roles and groups claims,
-- a JSON array of strings) and whether that snapshot is partial. The sign-in gate writes the '' row
-- before a session is issued, and the token exchange writes its portal's row before a delegated token is
-- issued; either is refused when its row cannot be written. signed_in_at is the database time of that
-- write. issued_at is when the token it came from was issued (the subject token's iat for an exchange,
-- the sign-in's time for the gate), NULL for a row filled here: a token issued before the row's is not
-- written over it, so an older token cannot put back a group a newer one no longer carried. A running run's component grants are judged for its owner against the row of the door that
-- launched it. A SCIM removal from a group takes the group out of every row of the person the SCIM user is
-- bound to; erasing a person's credentials, and a leaver purge, delete all their rows. No secret.
--
-- Filled here, as '' rows, from each person's key-domain login snapshot (0121's key_domain_login_groups,
-- written at the same sign-ins, best-effort) and the email of the identity row they last signed in with
-- (0113's principal_identities), so a person who signed in before this table existed is known without
-- signing in again.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the
-- owner that holds DELETE on key_domain_login_groups, which the same sign-ins write and the same erasures
-- clear, gets SELECT, INSERT, UPDATE and DELETE on principal_groups.
CREATE TABLE IF NOT EXISTS principal_groups (
    principal    TEXT        NOT NULL CHECK (principal <> ''),
    portal       TEXT        NOT NULL DEFAULT '',
    email        TEXT        NOT NULL DEFAULT '',
    groups       JSONB       NOT NULL DEFAULT '[]',
    truncated    BOOLEAN     NOT NULL,
    signed_in_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    issued_at    TIMESTAMPTZ,
    PRIMARY KEY (principal, portal)
);

INSERT INTO principal_groups (principal, email, groups, truncated, signed_in_at)
SELECT g.principal,
       COALESCE((SELECT i.email_lower FROM principal_identities i
                  WHERE i.principal = g.principal
                  ORDER BY i.last_login_at DESC NULLS LAST, i.created_at DESC
                  LIMIT 1), ''),
       g.groups, g.truncated, g.verified_at
  FROM key_domain_login_groups g
ON CONFLICT (principal, portal) DO NOTHING;

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'key_domain_login_groups'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON principal_groups TO %I', r.rolname);
    END LOOP;
END
$mig$;
