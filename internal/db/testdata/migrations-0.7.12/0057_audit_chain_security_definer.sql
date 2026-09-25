-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Run the chain trigger as its OWNER, so the documented split app role keeps
-- working (review finding A1 against 0056).
--
-- 0056 moved seq allocation into audit_events_chain() to make chain order and
-- seq order one decision. What that quietly changed is WHO is allowed to make
-- it. `seq` is GENERATED ALWAYS AS IDENTITY (0001), and an identity DEFAULT is
-- a NextValueExpr the planner evaluates WITHOUT checking sequence privileges;
-- an ordinary nextval() call in a plpgsql body is a function call by the
-- INVOKING role, and it does check USAGE. The deploy posture Wardyn documents
-- and logs (0007_audit_least_privilege.sql, cmd/wardynd's boot check) is an app
-- role holding INSERT and SELECT on audit_events and nothing else — so after
-- 0056 that role got:
--
--     ERROR: permission denied for sequence audit_events_seq_seq
--     CONTEXT: PL/pgSQL function audit_events_chain() line 11 at assignment
--
-- on EVERY audit insert: every request's audit write falls to the spool (which
-- then cannot drain, because the store rejects every line), and every
-- credential mint is refused, since the broker's in-tx audit insert is fatal to
-- the mint. A split-role deployment would have upgraded into that.
--
-- The fix is not to hand the app role another grant. Granting USAGE on the
-- sequence would widen what an INSERT-only role may do (nextval() by hand, on
-- the column that binds every chain position), and every app role created after
-- this migration would have to remember it. Instead the function becomes
-- SECURITY DEFINER: it runs as its owner — the role that ran this migration,
-- which also owns audit_events — so the head read and the seq allocation
-- succeed no matter what the caller was granted. Nothing is widened for the
-- caller: a trigger function cannot be invoked directly (Postgres refuses,
-- "trigger functions can only be called as triggers"), so the only way to reach
-- this body is still an INSERT that the caller's own table privileges allowed.
--
-- SET search_path is mandatory for a SECURITY DEFINER function and not
-- boilerplate: without it the body resolves audit_row_hash, nextval and the
-- table through the CALLER's search_path, and anyone able to create a schema
-- ahead of it could shadow them with their own definitions running as the
-- owner. Pinning it also settles the unqualified-resolution note the F11 trace
-- raised (H16) for the one function that now runs with elevated rights. Every
-- object 0047/0056 create is unqualified, i.e. in the schema the migrations ran
-- against; pg_catalog first, then public, matches that.
--
-- The sequence is resolved through TG_RELID rather than the literal table name,
-- so the lookup follows the trigger to whatever table (and schema) it is
-- attached to instead of hard-coding one.
--
-- NOT a data migration: no row changes, no re-chaining. The trigger is
-- re-created (idempotent DROP/CREATE) so a database whose trigger went missing
-- between 0056 and here comes back attached to this definition.

CREATE OR REPLACE FUNCTION audit_events_chain() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    head TEXT;
BEGIN
    -- db.AuditChainLockKey (ASCII "WARDYCHA"). Written as a literal because a
    -- migration cannot import Go; db.go's doc comment names this file's
    -- predecessor, 0056.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    -- Position is allocated HERE, under the lock, not by the identity default
    -- that already ran (0056). Running as the definer is what keeps that legal
    -- for an app role that holds no privilege on the sequence.
    NEW.seq := nextval(pg_get_serial_sequence(TG_RELID::regclass::text, 'seq'));
    SELECT row_hash INTO head
      FROM audit_events
     WHERE row_hash IS NOT NULL
     ORDER BY seq DESC
     LIMIT 1;
    NEW.prev_hash := head;
    -- "time" is quoted because it is a Postgres type name.
    NEW.row_hash := audit_row_hash(head, NEW.id, NEW."time", NEW.run_id,
        NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
        NEW.source_ip, NEW.data);
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS audit_events_chain ON audit_events;
CREATE TRIGGER audit_events_chain
    BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_chain();
