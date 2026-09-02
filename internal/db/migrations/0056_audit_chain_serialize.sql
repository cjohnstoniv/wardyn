-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Serialize audit_events appends IN THE DATABASE (F11 H5).
--
-- 0047 left the serializing lock to the CALLER, and stated why the trigger
-- could not take it: by the time a BEFORE INSERT trigger runs, the identity
-- default has already handed NEW its seq, so two racing writers could take a
-- lock inside the trigger in the opposite order to their seq allocation and
-- leave chain order and seq order inverted — which the verify sweep reports as
-- tampering. That reasoning is correct as far as it goes, and its consequence
-- was that only the two in-tree writers (store.InsertAuditEvent and the
-- broker's insertAuditEventTx) serialized at all. ANY other INSERT — psql, a
-- seed script, a test helper, a future code path — read the same head as a
-- concurrent locked writer, both rows chained to it, and the sweep then
-- reported "prev_hash does not match the preceding row (a row was deleted or
-- reordered)" at the LOCKED writer's row although nothing had been altered.
-- A false tamper verdict on an append-only log is worse than a missing one:
-- docs/OPERATIONS.md tells the operator to treat it as an incident, and the
-- chain gives them no way to clear it.
--
-- The way out of 0047's dilemma is to stop letting the identity default decide
-- position: the trigger takes the lock FIRST and then assigns NEW.seq itself,
-- from the same sequence, while holding it. Position and chain link are now
-- allocated under one lock, so chain order IS seq order by construction — for
-- every writer, not only the two that remember to lock. The identity default
-- still fires before the trigger and its value is discarded, which burns one
-- extra seq per insert; seq is a bigint, gaps are already expected (a rolled
-- back insert burns one too, and 0047 deliberately does not hash seq).
--
-- The callers KEEP their pg_advisory_xact_lock: advisory locks are re-entrant
-- within a transaction, so the trigger's acquisition is free for them, and
-- taking it before the statement keeps their head read and their INSERT inside
-- one lock hold exactly as before. What changes is that a writer which does NOT
-- take it can no longer fork the chain — it now WAITS for the writer in front
-- of it, which is what "one chain, one head" means. A writer that holds a
-- transaction open across another writer's append will therefore block that
-- append until it commits; that is the price of a linear chain, and it is paid
-- by the un-serialized writer's own long transaction.
--
-- NOT a data migration: nothing already written changes, and no row is
-- re-chained. Rows written before this migration keep the links they were
-- given.

CREATE OR REPLACE FUNCTION audit_events_chain() RETURNS trigger AS $$
DECLARE
    head TEXT;
BEGIN
    -- db.AuditChainLockKey (ASCII "WARDYCHA"). Written as a literal because a
    -- migration cannot import Go; db.go's doc comment names this file.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    -- Position is allocated HERE, under the lock, not by the identity default
    -- that already ran. pg_get_serial_sequence resolves the identity column's
    -- own sequence, so nothing hard-codes its generated name.
    NEW.seq := nextval(pg_get_serial_sequence('audit_events', 'seq'));
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
$$ LANGUAGE plpgsql;

-- Idempotent re-create, so a deployment whose trigger was dropped between 0047
-- and this migration gets it back with the new function attached.
DROP TRIGGER IF EXISTS audit_events_chain ON audit_events;
CREATE TRIGGER audit_events_chain
    BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_chain();
