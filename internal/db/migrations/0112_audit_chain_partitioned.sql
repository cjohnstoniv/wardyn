-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The replayable definition of the partitioned audit_events guards (0.8.6 audit retention;
-- design record docs/design/0.8/0.8.6-ar.md, "M-A2: the replayable trigger definition").
--
-- 0111 converted audit_events and installed working guards before it committed. This file
-- is the same guards as IDEMPOTENT text: db.replayTriggerMigrations re-runs every migration
-- that spells the chain trigger's name, in filename order, to restore a trigger an owner
-- dropped, and 0111 deliberately does not spell it (re-running a conversion would refuse
-- boot). So this is the file that is replayed, AFTER 0047/0056/0057/0058 have put their
-- older function bodies and triggers back, and it puts the partition-aware ones back last.
--
-- The chain function VERIFIES the position audit_append() allocated; it never allocates and
-- never calls pg_get_serial_sequence(TG_RELID), which is NULL on a partition. Its body is
-- the one 0111 defines; a test compares the two so they cannot drift.
--
-- What it creates, on the parent: the chain trigger (BEFORE INSERT, row level), the
-- append-only trigger (BEFORE UPDATE OR DELETE, row level) and a TRUNCATE trigger; Postgres
-- clones the row-level pair onto every partition. A statement-level TRUNCATE trigger is not
-- cloned, so it is also created on each partition.

DO $mig$
DECLARE
    ns   text;
    nsq  text;
    r    record;
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
SET search_path = pg_catalog, @ns@, pg_temp
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
    -- The chain head is the newest hashed row, read from the table exactly as 0058 reads it. (The
    -- high-water mark is not the head: it is what verify compares the newest row against.)
    SELECT t.row_hash INTO head
      FROM @ns@.audit_events t
     WHERE t.row_hash IS NOT NULL
     ORDER BY t.seq DESC
     LIMIT 1;
    NEW.prev_hash := head;
    -- "time" is quoted because it is a Postgres type name.
    NEW.row_hash := @ns@.audit_row_hash(head, NEW.id, NEW."time", NEW.run_id,
        NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
        NEW.source_ip, NEW.data);
    RETURN NEW;
END;
$body$
$fn$, '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_events_chain() FROM PUBLIC', '@ns@', nsq);

    -- The parent's triggers. Dropping a parent trigger drops its clones, and creating it
    -- clones it onto every existing partition again.
    EXECUTE format('DROP TRIGGER IF EXISTS audit_events_chain ON %I.audit_events', ns);
    EXECUTE format('CREATE TRIGGER audit_events_chain BEFORE INSERT ON %1$I.audit_events FOR EACH ROW EXECUTE FUNCTION %1$I.audit_events_chain()', ns);
    EXECUTE format('DROP TRIGGER IF EXISTS audit_events_no_update ON %I.audit_events', ns);
    EXECUTE format('CREATE TRIGGER audit_events_no_update BEFORE UPDATE OR DELETE ON %1$I.audit_events FOR EACH ROW EXECUTE FUNCTION %1$I.audit_events_append_only()', ns);
    EXECUTE format('DROP TRIGGER IF EXISTS audit_events_no_truncate ON %I.audit_events', ns);
    EXECUTE format('CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON %1$I.audit_events FOR EACH STATEMENT EXECUTE FUNCTION %1$I.audit_events_append_only()', ns);

    -- A TRUNCATE trigger on each partition (a statement trigger is not cloned).
    FOR r IN SELECT i.inhrelid::regclass::text AS part
               FROM pg_catalog.pg_inherits i
              WHERE i.inhparent = format('%I.audit_events', ns)::regclass
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS audit_events_no_truncate ON %s', r.part);
        EXECUTE format('CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON %1$s FOR EACH STATEMENT EXECUTE FUNCTION %2$I.audit_events_append_only()', r.part, ns);
    END LOOP;
END
$mig$;
