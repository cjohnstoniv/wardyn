-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_support_grants: one row for each time an admin asks to help inside someone else's running run, and what
-- the run's owner answered. A row is requested until its owner accepts it, naming the doors the helper may use
-- (doors, a subset of doors_asked) and for how long (minutes: 15, 30 or 60); it is open until ended_at is set,
-- and a run has at most one open row. Every time is the database's: requested_at, accepted_at and expires_at
-- are written with now(), and a replica's clock decides nothing about access. request_rev_seq and
-- accept_rev_seq are the revocation sequence each credential recorded, so a later sign-out of the helper or the
-- owner ends the grant. end_audited is false for an end that no route wrote an audit row for: the sweep claims
-- those and writes it. reason is what the helper gave, helper_email and owner_email the verified emails a
-- revoke may name; erasing a person blanks all three.
--
-- attach_tickets gains support_grant and support_expires, the grant a ticket was minted under and when that
-- grant ends, and run_attach_leases gains support, which is true for a lease held under a grant.
--
-- run_support_grants_end_with_run() ends a run's open grant in the same statement that stops the run or marks it
-- lost, with end_reason 'run_ended'. A revive later clears lost_at, and the grant stays ended. The function runs
-- as its owner, the migrator, so that a split-role install whose app role has not been given this table still
-- ends the grant and still writes the run: the trigger must never fail a run write. It is written as 0285
-- writes the audit functions: the schema read from the table, every name in it, and a path of pg_catalog then
-- pg_temp. Never a secret.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds DELETE on run_attach_leases, which the same lease and ticket writes go through, gets SELECT,
-- INSERT, UPDATE and DELETE on run_support_grants. The new columns ride the grants their tables already have.
CREATE TABLE IF NOT EXISTS run_support_grants (
    id              UUID        PRIMARY KEY,
    run_id          UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    helper          TEXT        NOT NULL CHECK (helper <> ''),
    helper_email    TEXT        NOT NULL DEFAULT '',
    owner_email     TEXT        NOT NULL DEFAULT '',
    reason          TEXT        NOT NULL CHECK (length(reason) <= 500),
    doors_asked     TEXT[]      NOT NULL CHECK (cardinality(doors_asked) >= 1),
    doors           TEXT[]      CHECK (doors IS NULL OR doors <@ doors_asked),
    minutes         INT         CHECK (minutes IS NULL OR minutes IN (15, 30, 60)),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_rev_seq BIGINT      NOT NULL DEFAULT 0,
    accepted_at     TIMESTAMPTZ,
    accept_rev_seq  BIGINT      NOT NULL DEFAULT 0,
    expires_at      TIMESTAMPTZ,
    last_seen_at    TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    end_reason      TEXT,
    end_audited     BOOLEAN     NOT NULL DEFAULT false,
    CHECK ((accepted_at IS NULL) = (expires_at IS NULL) AND (accepted_at IS NULL) = (minutes IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS run_support_grants_one_open ON run_support_grants (run_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS run_support_grants_helper ON run_support_grants (helper);

ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS support_grant UUID,
                           ADD COLUMN IF NOT EXISTS support_expires TIMESTAMPTZ;
ALTER TABLE run_attach_leases ADD COLUMN IF NOT EXISTS support BOOLEAN NOT NULL DEFAULT false;

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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_support_grants TO %I', r.rolname);
    END LOOP;
END
$mig$;

DO $mig$
DECLARE
    ns text;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'run_support_grants'::regclass;

    EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.run_support_grants_end_with_run() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
BEGIN
    UPDATE %1$I.run_support_grants SET ended_at = pg_catalog.now(), end_reason = 'run_ended'
     WHERE run_id = NEW.id AND ended_at IS NULL;
    RETURN NULL;
END;
$body$
$fn$, ns);

    EXECUTE pg_catalog.format('DROP TRIGGER IF EXISTS run_support_grants_end_with_run ON %1$I.agent_runs', ns);
    EXECUTE pg_catalog.format($tr$
CREATE TRIGGER run_support_grants_end_with_run
    AFTER UPDATE OF state, lost_at ON %1$I.agent_runs
    FOR EACH ROW
    WHEN (NEW.state <> 'RUNNING' OR NEW.lost_at IS NOT NULL)
    EXECUTE FUNCTION %1$I.run_support_grants_end_with_run()
$tr$, ns);
END
$mig$;
