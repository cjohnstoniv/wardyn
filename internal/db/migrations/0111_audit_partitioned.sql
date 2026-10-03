-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Convert audit_events into a monthly range-partitioned table on a server-assigned
-- recorded_at, and move every insert behind one database function, audit_append()
-- (0.8.6 audit retention; design record docs/design/0.8/0.8.6-ar.md, "Data model
-- and migrations" and "The append protocol and privileges").
--
-- WHY: retention drops whole monthly partitions, oldest first. That is only safe if
-- every partition is a CONTIGUOUS PREFIX of chain order, so dropping the oldest never
-- removes an interior link. Partitioning on the event's own "time" cannot give that
-- (the audit spool replays a failed write at-least-once with its ORIGINAL time), and a
-- column DEFAULT now() cannot either (now() is the transaction START time, so a
-- transaction that began before a month boundary can take the chain lock after one from
-- the new month). So the position is decided in chain order, under the chain lock, by
-- audit_append(): it allocates seq, sets recorded_at := greatest(clock_timestamp(),
-- high-water recorded_at), and inserts with those explicit values. The chain trigger
-- (audit_events_chain, defined again in 0112 as the replayable definition) VERIFIES them.
--
-- ONE TRANSACTION, WITH WORKING GUARDS BEFORE COMMIT. The migrator commits each file on
-- its own and only runs ensureAuditTriggers after all of them, so a conversion split
-- over two files (drop the guards in one, create them in the next) would leave a crash
-- window with an unprotected table. This file drops the old triggers and installs the
-- new ones in the same transaction; 0112 is only the idempotent, replayable definition.
--
-- THE TRIGGER'S NAME IS NEVER WRITTEN HERE AS DDL. db.replayTriggerMigrations re-runs
-- every migration whose text contains "TRIGGER" followed by the chain trigger's name, to
-- restore a trigger an owner dropped. Re-running THIS file would re-run the conversion
-- and refuse boot. So the three old triggers are dropped, and the new ones created, with
-- EXECUTE format('... TRIGGER %I ...') and the name passed as a parameter, and 0112 (which
-- is safe to replay) is the only file that spells it out. A test pins this.
--
-- STOPPED WRITERS. 0.8.5 inserts directly into audit_events with no seq or recorded_at;
-- after this file the chain trigger refuses those rows. The chart is Recreate, so Migrate
-- at the new pod's boot is the stopped-writer path.
--
-- ORDER (the L1.1 upgrade test runs exactly this against a populated 0.8.5 schema):
--   T0 := transaction_timestamp(); ADD COLUMN recorded_at DEFAULT '<T0>' (a fast
--   default: every existing row reads T0); move seq onto a standalone sequence and DROP
--   IDENTITY; drop the three triggers; drop the PK; rename to audit_events_legacy; CHECK
--   the legacy bound; create the parent, its PK and indexes; ATTACH legacy; copy grants;
--   persist the cutover; create the first live partition and twelve months ahead; install
--   the guards and the privilege model.
--
-- Postgres 13 is the floor (0107): BEFORE ROW triggers on a partitioned table exist from 13.

DO $mig$
DECLARE
    ns         text;
    nsq        text;
    t0         timestamptz := transaction_timestamp();
    cutover    timestamptz;
    legacy_oid oid;
    last_seq   bigint;
    hw_hash    text;
    pk_name    text;
    r          record;
    trg        text;
    rd         text;
    readers    text[] := ARRAY[]::text[];
    writers    text[] := ARRAY[]::text[];
    -- The trigger names, as DATA: see the header for why they are never spelled as DDL here.
    trg_chain  constant text := 'audit_events_chain';
    trg_update constant text := 'audit_events_no_update';
    trg_trunc  constant text := 'audit_events_no_truncate';
BEGIN
    SELECT n.nspname, c.oid INTO ns, legacy_oid
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass AND c.relkind = 'r';
    IF ns IS NULL THEN
        RAISE EXCEPTION 'audit_events is not an ordinary table: it was already converted, or was never created';
    END IF;
    nsq := quote_ident(ns);
    cutover := t0 + interval '1 microsecond';

    -- Everything below that reads the old table does it BEFORE the primary key is dropped,
    -- so the head and the high-water mark are index probes rather than table scans.
    EXECUTE replace('SELECT coalesce(max(seq), 0) FROM @ns@.audit_events', '@ns@', nsq) INTO last_seq;
    last_seq := greatest(last_seq, coalesce(
        pg_sequence_last_value(pg_get_serial_sequence(format('%I.audit_events', ns), 'seq')::regclass), 0));
    EXECUTE replace($q$SELECT row_hash FROM @ns@.audit_events
                       WHERE row_hash IS NOT NULL ORDER BY seq DESC LIMIT 1$q$, '@ns@', nsq) INTO hw_hash;

    -- Who may do what today, read from the catalog (never from a role name): every role that
    -- holds INSERT is granted EXECUTE on the new functions before INSERT is taken away, and
    -- every role that can SELECT keeps SELECT on the parent. The owner and PUBLIC are skipped:
    -- the owner needs no grant, and PUBLIC gets nothing here.
    FOR r IN
        SELECT ro.rolname::text AS rolname,
               bool_or(a.privilege_type = 'SELECT') AS can_select,
               bool_or(a.privilege_type = 'INSERT') AS can_insert
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = legacy_oid AND a.grantee <> c.relowner
         GROUP BY ro.rolname
    LOOP
        IF r.can_select THEN readers := readers || r.rolname; END IF;
        IF r.can_insert THEN writers := writers || r.rolname; END IF;
    END LOOP;

    -- 1. recorded_at: a constant default, so no rewrite; every existing row reads T0.
    EXECUTE format('ALTER TABLE %I.audit_events ADD COLUMN recorded_at timestamptz NOT NULL DEFAULT %L', ns, t0);

    -- 2. seq moves to a standalone sequence that continues after the identity's last value.
    EXECUTE format('CREATE SEQUENCE %I.audit_events_seq START WITH %s', ns, last_seq + 1);
    EXECUTE format('ALTER TABLE %I.audit_events ALTER COLUMN seq DROP IDENTITY', ns);

    -- 3. The three old triggers, by parameter (never as literal DDL).
    FOREACH trg IN ARRAY ARRAY[trg_chain, trg_update, trg_trunc] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I.audit_events', trg, ns);
    END LOOP;

    -- 4. Drop the primary key, rename the table and each index to a _legacy name.
    SELECT conname INTO pk_name FROM pg_catalog.pg_constraint WHERE conrelid = legacy_oid AND contype = 'p';
    IF pk_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE %I.audit_events DROP CONSTRAINT %I', ns, pk_name);
    END IF;
    EXECUTE format('ALTER TABLE %I.audit_events RENAME TO audit_events_legacy', ns);
    FOR r IN SELECT c.relname::text AS relname
               FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
              WHERE i.indrelid = legacy_oid
    LOOP
        EXECUTE format('ALTER INDEX %I.%I RENAME TO %I', ns, r.relname, r.relname || '_legacy');
    END LOOP;

    -- 6. The partitioned parent, its primary key and its indexes. The indexes keep their
    -- original names (the legacy ones were renamed above, so nothing collides), none is
    -- created IF NOT EXISTS (a collision must fail loudly), and 0047's chain-head index keeps
    -- the very name 0047 re-creates under IF NOT EXISTS when db.replayTriggerMigrations
    -- replays it, so a replay finds it and adds nothing.
    EXECUTE format($q$CREATE TABLE %1$I.audit_events (LIKE %1$I.audit_events_legacy INCLUDING DEFAULTS INCLUDING CONSTRAINTS)
                      PARTITION BY RANGE (recorded_at)$q$, ns);
    EXECUTE format('ALTER TABLE %I.audit_events ALTER COLUMN recorded_at SET DEFAULT now()', ns);
    EXECUTE format('ALTER TABLE %I.audit_events ADD CONSTRAINT audit_events_pkey PRIMARY KEY (seq, recorded_at)', ns);
    EXECUTE replace($q$CREATE INDEX audit_events_run_idx ON @ns@.audit_events (run_id)$q$, '@ns@', nsq);
    EXECUTE replace($q$CREATE INDEX audit_events_time_idx ON @ns@.audit_events (time)$q$, '@ns@', nsq);
    EXECUTE replace($q$CREATE INDEX audit_events_chain_head_idx ON @ns@.audit_events (seq DESC)
                       WHERE row_hash IS NOT NULL$q$, '@ns@', nsq);
    EXECUTE replace($q$CREATE INDEX audit_events_action_seq_idx ON @ns@.audit_events (action, seq DESC)$q$, '@ns@', nsq);
    EXECUTE replace($q$CREATE INDEX audit_events_run_seq_idx ON @ns@.audit_events (run_id, seq)$q$, '@ns@', nsq);
    EXECUTE replace($q$CREATE INDEX audit_events_device_origin_idx ON @ns@.audit_events
                       ((data->'device_origin'->>'device_id'), (data->'device_origin'->>'seq'), seq)
                       WHERE data ? 'device_origin'$q$, '@ns@', nsq);

    -- 6b. The legacy bound, added AFTER the parent was created so LIKE ... INCLUDING CONSTRAINTS
    -- could not copy it onto the parent. ATTACH then skips its own validation scan; this
    -- statement scans the legacy table once instead.
    EXECUTE format('ALTER TABLE %I.audit_events_legacy ADD CONSTRAINT legacy_bound CHECK (recorded_at < %L)', ns, cutover);

    -- 7. ATTACH the legacy table. Postgres adopts the matching legacy indexes and builds only
    -- the (seq, recorded_at) unique index: the one index build of the conversion.
    EXECUTE format('ALTER TABLE %1$I.audit_events ATTACH PARTITION %1$I.audit_events_legacy FOR VALUES FROM (MINVALUE) TO (%2$L)', ns, cutover);

    -- 8. The high-water mark, the expected-partition manifest and the cutover.
    EXECUTE replace($q$CREATE TABLE @ns@.audit_partition_meta (
            singleton      boolean PRIMARY KEY DEFAULT true CHECK (singleton),
            cutover        timestamptz NOT NULL,
            hw_seq         bigint      NOT NULL,
            hw_recorded_at timestamptz NOT NULL,
            hw_row_hash    text,
            manifest       jsonb       NOT NULL DEFAULT '[]'::jsonb
        )$q$, '@ns@', nsq);
    EXECUTE format('INSERT INTO %I.audit_partition_meta (cutover, hw_seq, hw_recorded_at, hw_row_hash) VALUES (%L, %s, %L, %L)',
                   ns, cutover, last_seq, t0, hw_hash);

    -- 9. Attested retention drops and legacy splits are recorded here by ar-l1.3 and ar-l1.7;
    -- the table exists, empty, so the shape is fixed before either lane lands.
    EXECUTE replace($q$CREATE TABLE @ns@.audit_chain_anchors (
            id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            kind           text        NOT NULL CHECK (kind IN ('drop', 'split')),
            partition_name text        NOT NULL,
            seq_lo         bigint,
            seq_hi         bigint,
            recorded_lo    timestamptz,
            recorded_hi    timestamptz,
            row_count      bigint,
            digest         text,
            tail_row_hash  text,
            actor          text,
            anchored_at    timestamptz NOT NULL DEFAULT now(),
            event_seq      bigint
        )$q$, '@ns@', nsq);

    -- The functions. Every name is qualified with the ACTUAL schema, discovered from the
    -- catalog above, and every pinned path ends with pg_temp (0058: an app-owned temporary
    -- table can shadow an unqualified relation, and a non-public install resolves wrongly).

    -- audit_harden_relation: REVOKE ALL from every non-owner grantee of a relation (a default
    -- privilege may have granted the app role write rights on a table the migrator just
    -- created), then GRANT SELECT to the roles that may read the log. SECURITY INVOKER on
    -- purpose: it only ever runs inside the owner's own SECURITY DEFINER functions or this
    -- migration, and holds no privilege of its own.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_harden_relation(p_rel regclass, p_readers text[]) RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
DECLARE
    g record;
    rd text;
BEGIN
    FOR g IN
        SELECT a.grantee, (SELECT ro.rolname::text FROM pg_catalog.pg_roles ro WHERE ro.oid = a.grantee) AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
         WHERE c.oid = p_rel AND a.grantee <> c.relowner
         GROUP BY a.grantee
    LOOP
        IF g.grantee = 0 THEN
            EXECUTE format('REVOKE ALL ON %s FROM PUBLIC', p_rel);
        ELSE
            EXECUTE format('REVOKE ALL ON %s FROM %I', p_rel, g.rolname);
        END IF;
    END LOOP;
    EXECUTE format('REVOKE ALL ON %s FROM PUBLIC', p_rel);
    FOREACH rd IN ARRAY coalesce(p_readers, ARRAY[]::text[]) LOOP
        EXECUTE format('GRANT SELECT ON %s TO %I', p_rel, rd);
    END LOOP;
END;
$body$
$fn$, '@ns@', nsq);

    -- audit_events_chain: the chain trigger. Defined here so the guards work at commit, and
    -- again, identically, in 0112 (a test compares the two bodies). It VERIFIES the position
    -- audit_append() allocated; it never allocates, and it never calls
    -- pg_get_serial_sequence(TG_RELID) (NULL on a partition).
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

    -- audit_append: the ONLY way a row enters the log.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_append(
    p_id uuid, p_time timestamptz, p_run_id uuid, p_actor_type text, p_actor text,
    p_action text, p_target text, p_outcome text, p_source_ip text, p_data jsonb)
RETURNS TABLE (seq bigint, recorded_at timestamptz, prev_hash text, row_hash text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
DECLARE
    m      @ns@.audit_partition_meta%ROWTYPE;
    v_seq  bigint;
    v_at   timestamptz;
    v_prev text;
    v_hash text;
BEGIN
    -- db.AuditChainLockKey: position, timestamp and chain link are decided under ONE lock.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    SELECT * INTO m FROM @ns@.audit_partition_meta;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit_append: audit_partition_meta has no row';
    END IF;
    v_seq := nextval('@ns@.audit_events_seq');
    -- Read from the high-water mark, never from the head row, so a stray row cannot drag the
    -- clock; greatest() keeps recorded_at monotonic under a clock step backwards.
    v_at := greatest(clock_timestamp(), m.hw_recorded_at, m.cutover);
    PERFORM pg_catalog.set_config('wardyn.audit_seq', v_seq::text, true);
    INSERT INTO @ns@.audit_events
        (seq, id, "time", run_id, actor_type, actor, action, target, outcome, source_ip, data, recorded_at)
    VALUES
        (v_seq, p_id, p_time, p_run_id, p_actor_type, p_actor, p_action, p_target, p_outcome, p_source_ip, p_data, v_at)
    RETURNING audit_events.prev_hash, audit_events.row_hash INTO v_prev, v_hash;
    UPDATE @ns@.audit_partition_meta
       SET hw_seq = v_seq, hw_recorded_at = v_at, hw_row_hash = v_hash;
    RETURN QUERY SELECT v_seq, v_at, v_prev, v_hash;
END;
$body$
$fn$, '@ns@', nsq);

    -- audit_ensure_partitions: create the months ahead. Standalone table + CHECK + ATTACH,
    -- never PARTITION OF (ACCESS EXCLUSIVE on the parent); ATTACH takes only SHARE UPDATE
    -- EXCLUSIVE and the CHECK lets it skip its validation scan. It continues from the highest
    -- upper bound in the manifest, never from date_trunc(now()). No DEFAULT partition: a row
    -- in one would make its month impossible to attach later.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_ensure_partitions(p_months integer) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, @ns@, pg_temp
SET lock_timeout = '5s'
AS $body$
DECLARE
    m        @ns@.audit_partition_meta%ROWTYPE;
    hi_cur   timestamptz;
    lo       timestamptz;
    hi       timestamptz;
    target   timestamptz;
    pname    text;
    v_manifest jsonb;
    readers  text[];
    created  integer := 0;
BEGIN
    IF p_months IS NULL OR p_months < 1 OR p_months > 24 THEN
        RAISE EXCEPTION 'audit_ensure_partitions: p_months must be between 1 and 24, got %', p_months;
    END IF;
    -- db.AuditPartitionLockKey (ASCII "WARDYAPT"): one creator at a time.
    PERFORM pg_advisory_xact_lock(6287397008294629460);
    SELECT * INTO m FROM @ns@.audit_partition_meta;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit_ensure_partitions: audit_partition_meta has no row';
    END IF;
    v_manifest := m.manifest;
    SELECT coalesce(max((e->>'hi')::timestamptz), m.cutover) INTO hi_cur FROM jsonb_array_elements(v_manifest) e;
    -- The current month plus p_months after it, in UTC.
    target := (date_trunc('month', clock_timestamp() AT TIME ZONE 'UTC') + make_interval(months => p_months + 1)) AT TIME ZONE 'UTC';
    SELECT coalesce(array_agg(ro.rolname::text), ARRAY[]::text[]) INTO readers
      FROM (SELECT DISTINCT a.grantee
              FROM pg_catalog.pg_class c
              CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
             WHERE c.oid = '@ns@.audit_events'::regclass AND a.privilege_type = 'SELECT'
               AND a.grantee <> c.relowner AND a.grantee <> 0) g
      JOIN pg_catalog.pg_roles ro ON ro.oid = g.grantee;
    WHILE hi_cur < target LOOP
        lo := hi_cur;
        hi := (date_trunc('month', lo AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC';
        pname := 'audit_events_p' || to_char(lo AT TIME ZONE 'UTC', 'YYYYMM');
        EXECUTE format('CREATE TABLE @ns@.%I (LIKE @ns@.audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', pname);
        EXECUTE format('ALTER TABLE @ns@.%I ADD CONSTRAINT %I CHECK (recorded_at >= %L AND recorded_at < %L)',
                       pname, pname || '_range', lo, hi);
        EXECUTE format('CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON @ns@.%I FOR EACH STATEMENT EXECUTE FUNCTION @ns@.audit_events_append_only()', pname);
        -- An operator's ENABLE ALWAYS on the parent's TRUNCATE trigger (the one trigger Postgres does
        -- not clone) is carried to the new partition's copy.
        IF (SELECT t.tgenabled FROM pg_catalog.pg_trigger t
             WHERE t.tgrelid = '@ns@.audit_events'::regclass AND t.tgname = 'audit_events_no_truncate') = 'A' THEN
            EXECUTE format('ALTER TABLE @ns@.%I ENABLE ALWAYS TRIGGER audit_events_no_truncate', pname);
        END IF;
        PERFORM @ns@.audit_harden_relation(('@ns@.' || quote_ident(pname))::regclass, readers);
        EXECUTE format('ALTER TABLE @ns@.audit_events ATTACH PARTITION @ns@.%I FOR VALUES FROM (%L) TO (%L)', pname, lo, hi);
        v_manifest := v_manifest || jsonb_build_array(jsonb_build_object(
            'name', pname,
            'lo', to_char(lo AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'hi', to_char(hi AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')));
        hi_cur := hi;
        created := created + 1;
    END LOOP;
    IF created > 0 THEN
        UPDATE @ns@.audit_partition_meta SET manifest = v_manifest;
    END IF;
    RETURN created;
END;
$body$
$fn$, '@ns@', nsq);

    -- Function privileges: nobody by default, then exactly the roles that could INSERT.
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_harden_relation(regclass, text[]) FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_events_chain() FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb) FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_ensure_partitions(integer) FROM PUBLIC', '@ns@', nsq);

    -- Parent and legacy: nothing but SELECT for the roles that had it.
    FOREACH rd IN ARRAY ARRAY['audit_events', 'audit_events_legacy', 'audit_partition_meta', 'audit_chain_anchors'] LOOP
        EXECUTE format('SELECT %I.audit_harden_relation($1, $2)', ns) USING format('%I.%I', ns, rd)::regclass, readers;
    END LOOP;

    -- The first live partition (FROM the cutover) and twelve months ahead.
    EXECUTE format('SELECT %I.audit_ensure_partitions(12)', ns);

    -- The privilege hand-over, in this transaction and in this order: EXECUTE first, and only
    -- then is INSERT gone (above: audit_harden_relation took it from the parent and legacy).
    FOREACH rd IN ARRAY writers LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb) TO %I', ns, rd);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_ensure_partitions(integer) TO %I', ns, rd);
    END LOOP;

    -- Guards, working before commit: the chain trigger and the append-only row trigger on the
    -- parent (cloned to every partition), and a TRUNCATE trigger on the parent and on each
    -- partition (a statement-level trigger is not cloned).
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT ON %I.audit_events FOR EACH ROW EXECUTE FUNCTION %I.audit_events_chain()', trg_chain, ns, ns);
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I.audit_events FOR EACH ROW EXECUTE FUNCTION %I.audit_events_append_only()', trg_update, ns, ns);
    EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I.audit_events FOR EACH STATEMENT EXECUTE FUNCTION %I.audit_events_append_only()', trg_trunc, ns, ns);
    FOR r IN SELECT i.inhrelid::regclass::text AS part
               FROM pg_catalog.pg_inherits i
              WHERE i.inhparent = format('%I.audit_events', ns)::regclass
    LOOP
        -- audit_ensure_partitions already armed the partitions it created; the legacy one is new.
        IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = r.part::regclass AND t.tgname = trg_trunc) THEN
            EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION %I.audit_events_append_only()', trg_trunc, r.part, ns);
        END IF;
    END LOOP;
END
$mig$;
