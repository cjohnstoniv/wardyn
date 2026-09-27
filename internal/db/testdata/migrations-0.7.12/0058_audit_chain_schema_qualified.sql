-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Resolve audit_events_chain()'s names by SCHEMA, not by search_path
-- (review findings F024 and F098 against 0057).
--
-- 0057 made the chain trigger SECURITY DEFINER — the right call, and the
-- reason the documented INSERT+SELECT app role can still append — but it
-- pinned `SET search_path = pg_catalog, public` while leaving the body's
-- references to `audit_events` and `audit_row_hash` UNQUALIFIED. That pin is
-- wrong in two directions at once, and review found both:
--
--   1. It hard-codes `public`. Nothing in Wardyn sets a search_path or
--      schema-qualifies a table: db.Migrate creates every object unqualified,
--      so Wardyn lives in whatever schema the connecting role's search_path
--      names. A deployment that puts it in its own schema — the ordinary shape
--      on a shared corporate Postgres, reachable with nothing but
--      `ALTER ROLE ... IN DATABASE ... SET search_path`, or even with stock
--      Postgres's default `"$user", public` when a same-named schema exists —
--      migrated cleanly through 0056 and worked. After 0057, Migrate still
--      reports success and then EVERY audit insert fails from inside the
--      trigger with `relation "audit_events" does not exist`: each request's
--      audit write falls to the spool, which then cannot drain because the
--      store rejects every line, and every credential mint is refused because
--      the broker's in-transaction audit insert is fatal to the mint. Where
--      `public` happens to hold a SECOND Wardyn schema it is quieter and
--      worse — the head read crosses into the wrong table and the chain
--      silently never links.
--
--   2. It omits `pg_temp`. Postgres searches the session temporary schema
--      FIRST for RELATION names whenever the path does not list it, and PUBLIC
--      holds TEMPORARY on a database by default. So a role that can connect
--      and INSERT — exactly the documented app role — could
--      `CREATE TEMP TABLE audit_events (seq bigint, row_hash text)`, seed it,
--      grant the definer SELECT on it, and have the definer-privileged head
--      read answer out of it: the caller then chooses NEW.prev_hash, and with
--      it the hash input of NEW.row_hash, on the log docs/OPERATIONS.md says
--      no caller can choose. PostgreSQL's own "Writing SECURITY DEFINER
--      Functions Safely" names a SET clause mentioning only trusted schemas as
--      the subvertible form and prescribes writing pg_temp LAST.
--
-- The fix is to stop depending on the path for name resolution at all. This
-- migration discovers the schema `audit_events` actually lives in — at apply
-- time, from the catalog, the same way the sequence lookup already follows
-- TG_RELID instead of a literal — and writes that schema into both the body's
-- references and the pinned path, with `pg_temp` LAST as the belt to the
-- qualification's braces. `public` is no longer named: on a public-schema
-- install the discovered schema IS public, and on any other install naming
-- public would only add a second place for a name to be found by accident.
--
-- NOT a data migration: no row changes, no re-chaining. CREATE OR REPLACE
-- keeps the function's OID, so an already-attached trigger picks up the new
-- body on its own; the trigger is re-created anyway (idempotent DROP/CREATE)
-- because db.replayTriggerMigrations selects the files that restore a MISSING
-- trigger by looking for "TRIGGER audit_events_chain" in them, and a replay
-- set that ended at 0057 would restore the very definition this migration
-- exists to replace.

DROP TRIGGER IF EXISTS audit_events_chain ON audit_events;

DO $mig$
DECLARE
    ns TEXT;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass;

    EXECUTE format($fmt$
CREATE OR REPLACE FUNCTION %1$I.audit_events_chain() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, %1$I, pg_temp
AS $fn$
DECLARE
    head TEXT;
BEGIN
    -- db.AuditChainLockKey (ASCII "WARDYCHA"). Written as a literal because a
    -- migration cannot import Go; db.go's doc comment names 0056.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    -- Position is allocated HERE, under the lock, not by the identity default
    -- that already ran (0056). Running as the definer is what keeps that legal
    -- for an app role that holds no privilege on the sequence (0057).
    NEW.seq := nextval(pg_get_serial_sequence(TG_RELID::regclass::text, 'seq'));
    SELECT row_hash INTO head
      FROM %1$I.audit_events
     WHERE row_hash IS NOT NULL
     ORDER BY seq DESC
     LIMIT 1;
    NEW.prev_hash := head;
    -- "time" is quoted because it is a Postgres type name.
    NEW.row_hash := %1$I.audit_row_hash(head, NEW.id, NEW."time", NEW.run_id,
        NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
        NEW.source_ip, NEW.data);
    RETURN NEW;
END;
$fn$
$fmt$, ns);

    EXECUTE format('CREATE TRIGGER audit_events_chain BEFORE INSERT ON %1$I.audit_events FOR EACH ROW EXECUTE FUNCTION %1$I.audit_events_chain()', ns);
END
$mig$;
