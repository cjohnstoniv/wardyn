-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- 0130's chain trigger read the newest hashed row from audit_events on every insert and then let the
-- recorded head (audit_partition_meta.hw_row_hash) override it. hw_row_hash is NULL only while the
-- log holds no hashed row, which is also when the table read returns nothing, so after the first row
-- the read was dead weight: a MergeAppend over every partition and its index, taken while every other
-- audit writer waits on the chain lock, growing with each retained month. The table is now read only
-- when hw_row_hash is NULL. Everything else in the function is 0130's.
--
-- Same idempotent shape as 0130: db.replayTriggerMigrations replays every file that spells the chain
-- trigger's name in filename order, so this body is the one a restored trigger is left with.

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
