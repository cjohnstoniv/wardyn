-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The chain function for hash formats 1 and 2: 0214's body, with the row hash picked by the row's
-- hash_format. A format 2 row with a salt that is not 64 lowercase hex, and any format other than 1 or 2,
-- is refused. This file holds only the function and the chain trigger's two statements, so it is pure DDL,
-- and it is the last file db.replayTriggerMigrations replays: a restored trigger is left with this body.
-- It needs the columns and audit_row_hash_v2 from the migration before it. Idempotent.

DO $mig$
DECLARE
    ns   text;
    nsq  text;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass;
    nsq := quote_ident(ns);

    EXECUTE replace($fn$
CREATE OR REPLACE FUNCTION @ns@.audit_events_chain() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
DECLARE
    m    @ns@.audit_partition_meta%ROWTYPE;
    cur  text;
    head text;
BEGIN
    -- db.AuditChainLockKey (ASCII "WARDYCHA"). A literal because a migration cannot import Go.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    cur := nullif(pg_catalog.current_setting('wardyn.audit_seq', true), '');
    IF cur IS NULL THEN
        RAISE EXCEPTION 'audit_events: rows are appended only through audit_append(); no position was allocated for this row';
    END IF;
    -- One allocation admits ONE row: consume it, so a multi-row INSERT is refused after its first row.
    PERFORM pg_catalog.set_config('wardyn.audit_seq', '', true);
    SELECT * INTO m FROM @ns@.audit_partition_meta;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit_events: audit_partition_meta has no row';
    END IF;
    IF NEW.seq IS DISTINCT FROM cur::bigint THEN
        RAISE EXCEPTION 'audit_events: seq % is not the position audit_append() allocated (%)', NEW.seq, cur;
    END IF;
    IF NEW.seq <= m.hw_seq THEN
        RAISE EXCEPTION 'audit_events: seq % is not above the chain high-water mark %', NEW.seq, m.hw_seq;
    END IF;
    IF NEW.recorded_at < m.cutover OR NEW.recorded_at < m.hw_recorded_at THEN
        RAISE EXCEPTION 'audit_events: recorded_at % is before the chain high-water mark', NEW.recorded_at;
    END IF;
    -- Forward only as far as the server clock plus a small skew. The high-water mark itself is
    -- always allowed: audit_append() pins to it when the clock steps backwards.
    IF NEW.recorded_at > greatest(clock_timestamp() + interval '5 seconds', m.hw_recorded_at) THEN
        RAISE EXCEPTION 'audit_events: recorded_at % is ahead of the server clock', NEW.recorded_at;
    END IF;
    -- The chain head is the recorded high-water hash: a row deleted from the tail must not let
    -- the next append re-link from its predecessor and look clean. The table is read only
    -- while no hash has been recorded yet, which is exactly when no hashed row exists to find; on
    -- the partitioned parent that read opens every partition and cannot prune.
    IF m.hw_row_hash IS NOT NULL THEN
        head := m.hw_row_hash;
    ELSE
        SELECT t.row_hash INTO head
          FROM @ns@.audit_events t
         WHERE t.row_hash IS NOT NULL
         ORDER BY t.seq DESC
         LIMIT 1;
    END IF;
    NEW.prev_hash := head;
    -- "time" is quoted because it is a Postgres type name.
    IF NEW.hash_format = 1 THEN
        NEW.row_hash := @ns@.audit_row_hash(head, NEW.id, NEW."time", NEW.run_id,
            NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
            NEW.source_ip, NEW.data);
    ELSIF NEW.hash_format = 2 THEN
        IF NEW.hash_salt IS NULL OR NEW.hash_salt !~ '^[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'audit_events: a format 2 row needs a hash_salt of 64 lowercase hex characters';
        END IF;
        NEW.row_hash := @ns@.audit_row_hash_v2(head, NEW.hash_salt, NEW.id, NEW."time", NEW.run_id,
            NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
            NEW.source_ip, NEW.data);
    ELSE
        RAISE EXCEPTION 'audit_events: unknown hash_format %', NEW.hash_format;
    END IF;
    RETURN NEW;
END;
$body$
$fn$, '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_events_chain() FROM PUBLIC', '@ns@', nsq);

    -- The parent's chain trigger. Dropping a parent trigger drops its clones, and creating it
    -- clones it onto every existing partition again.
    EXECUTE format('DROP TRIGGER IF EXISTS audit_events_chain ON %I.audit_events', ns);
    EXECUTE format('CREATE TRIGGER audit_events_chain BEFORE INSERT ON %1$I.audit_events FOR EACH ROW EXECUTE FUNCTION %1$I.audit_events_chain()', ns);
END
$mig$;
