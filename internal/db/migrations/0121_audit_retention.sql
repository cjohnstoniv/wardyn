-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The attested partition drop and the persisted retention policy (0.8.6 audit retention; design record
-- docs/design/0.8/0.8.6-ar.md, "Partition lifecycle", "Attested drop" and "Retention policy").
--
-- THE DROP IS A DATABASE FUNCTION. audit_retention_drop(p_partition, p_digest, p_actor) is the only way a
-- partition leaves the log. A Go-side DROP after the function returned would reopen the crash window the
-- function exists to close (a drop with no chained event and no anchor), and a DELETE is refused by the
-- append-only guard by design. In ONE transaction the function
--   1. refuses unless the partition is the OLDEST retained one, is CLOSED, is past the effective retention
--      window, and holds no rows of a run that is still live, and refuses unless the digest the operator
--      supplies equals the digest it recomputes (audit_partition_digest, 0120);
--   2. then, under the chain lock, appends the chained audit.retention.partition_dropped event through
--      audit_append, writes the audit_chain_anchors row (kind 'drop', row_count 0 for an empty partition:
--      verify anchors on the newest drop with row_count > 0), removes the partition from the expected
--      manifest, and DETACHes and DROPs it.
--
-- THE SCAN IS OUTSIDE THE LOCK. The digest, the row count and the tail hash are read of a CLOSED partition
-- (no append can route into it again, so it cannot change), before AuditChainLockKey is taken: computing
-- them under the lock would hold every audit writer for the length of a scan. The lock is taken only for
-- the drop itself, and the catalog checks are repeated under it (another drop may have run meanwhile).
--
-- THE POLICY IS DATABASE-OWNED. audit_retention_set_policy(p_days) computes the effect from clock_timestamp()
-- inside the database and is the only writer of the policy columns, which the app role cannot UPDATE
-- (audit_partition_meta is SELECT-only for it, 0111). An increase (including finite -> 0, forever) applies at
-- once. A decrease (including 0 -> finite) stores pending_days and pending_effective_at = now + 30 days, and
-- re-calling with the same value never resets the pair, so restarting does not restart the cooldown.
-- audit_retention_window() is the effective window both the drop and the status read: the pending value once
-- its date has passed, else the current one. 0 means forever.
--
-- EVERY REFUSAL IS A DISTINCT SQLSTATE (class WR, no standard class), so the API maps each to its own reason
-- without parsing text: WR001 not the oldest, WR002 not closed, WR003 inside the retention window (or
-- retention is forever), WR004 holds a live run's rows, WR005 digest mismatch, WR006 not a partition of
-- audit_events.
--
-- LOCK ORDER. The partition lock (AuditPartitionLockKey) first, then AuditChainLockKey, then the
-- audit_partition_meta row: audit_append holds the chain lock and then updates that row, so a function that
-- held the row and then waited for the chain lock would deadlock against it; audit_ensure_partitions takes
-- the partition lock and never the chain lock.
--
-- HARDENING is 0058's, as in 0111 and 0120: every name qualified with the ACTUAL schema discovered from the
-- catalog, the pinned search_path ends with pg_temp, partition names are validated against the parent's
-- pg_inherits before they are used as identifiers, EXECUTE is revoked from PUBLIC and granted to exactly the
-- roles that hold it on audit_append. The statements are DROP FUNCTION IF EXISTS then CREATE FUNCTION, never CREATE OR REPLACE.

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

    -- The policy columns. retention_days 0 is forever; a pending decrease is a (pending_days,
    -- pending_effective_at) pair, both set or both NULL.
    -- Idempotent (IF NOT EXISTS, a guarded constraint, DROP FUNCTION IF EXISTS before each CREATE): the
    -- migrator's break-glass and replay tests re-apply the newest migration over a migrated schema.
    EXECUTE replace($q$ALTER TABLE @ns@.audit_partition_meta
        ADD COLUMN IF NOT EXISTS retention_days        integer     NOT NULL DEFAULT 0 CHECK (retention_days >= 0),
        ADD COLUMN IF NOT EXISTS pending_days          integer     CHECK (pending_days >= 0),
        ADD COLUMN IF NOT EXISTS pending_effective_at  timestamptz,
        ADD COLUMN IF NOT EXISTS retention_set_at      timestamptz$q$,
        '@ns@', nsq);
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                    WHERE conrelid = format('%I.audit_partition_meta', ns)::regclass AND conname = 'audit_partition_meta_pending_pair') THEN
        EXECUTE replace($q$ALTER TABLE @ns@.audit_partition_meta
            ADD CONSTRAINT audit_partition_meta_pending_pair CHECK ((pending_days IS NULL) = (pending_effective_at IS NULL))$q$,
            '@ns@', nsq);
    END IF;

    -- The functions are re-created from scratch (and re-granted below).
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_drop(text, text, text)', '@ns@', nsq);
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_refuse(text, text)', '@ns@', nsq);
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_set_policy(integer)', '@ns@', nsq);
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_partitions(text, boolean)', '@ns@', nsq);
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_window()', '@ns@', nsq);

    -- audit_retention_window: the effective retention window in days (0 = forever).
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_window() RETURNS integer
LANGUAGE sql
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
    SELECT CASE WHEN m.pending_days IS NOT NULL AND m.pending_effective_at <= clock_timestamp()
                THEN m.pending_days ELSE m.retention_days END
      FROM @ns@.audit_partition_meta m
$body$
$fn$, '@ns@', nsq);

    -- audit_retention_partitions: every partition of the audit log, oldest first, with its state and whether
    -- (and if not, why not) the drop function would take it. The drop function asks this same function, so
    -- the status endpoint and the drop's refusals cannot disagree. p_name narrows to one partition (the
    -- oldest is still worked out among all of them); p_count false skips the row count, which is the one
    -- full scan of a partition and has no business under the chain lock.
    --
    -- SECURITY INVOKER: it only reads, and the roles that may read the log may read this.
    --
    -- The live-run set is the POSITIVE list types.NonTerminalRunStates, never a NOT IN of the terminal
    -- states: a state added later is then missed here (a drop that should have been refused) only until
    -- the Go test that pins the two together fails, not silently.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_partitions(p_name text, p_count boolean)
RETURNS TABLE (part_name text, part_lo timestamptz, part_hi timestamptz, part_rows bigint,
               part_state text, part_eligible boolean, part_refusal text)
LANGUAGE plpgsql
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
DECLARE
    v_parent oid := '@ns@.audit_events'::regclass;
    v_hw     timestamptz;
    v_days   integer;
    v_now    timestamptz := clock_timestamp();
    v_first  boolean := true;
    v_live   boolean;
    r        record;
BEGIN
    SELECT m.hw_recorded_at INTO v_hw FROM @ns@.audit_partition_meta m;
    v_days := @ns@.audit_retention_window();
    FOR r IN
        SELECT c.relname::text AS relname, n.nspname::text AS nsp,
               (regexp_match(b.bound, 'FROM \(''([^'']+)''\)'))[1]::timestamptz AS lo,
               (regexp_match(b.bound, 'TO \(''([^'']+)''\)'))[1]::timestamptz AS hi
          FROM pg_catalog.pg_inherits i
          JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
          CROSS JOIN LATERAL (SELECT pg_catalog.pg_get_expr(c.relpartbound, c.oid) AS bound) b
         WHERE i.inhparent = v_parent
         ORDER BY 3 NULLS FIRST, 1
    LOOP
        IF p_name IS NOT NULL AND r.relname <> p_name THEN
            v_first := false;
            CONTINUE;
        END IF;
        part_name := r.relname;
        part_lo := r.lo;
        part_hi := r.hi;
        part_rows := NULL;
        IF p_count THEN
            EXECUTE format('SELECT count(*) FROM %I.%I', r.nsp, r.relname) INTO part_rows;
        END IF;
        IF r.hi IS NOT NULL AND v_hw >= r.hi THEN
            part_state := 'closed';
        ELSIF r.lo IS NOT NULL AND v_hw < r.lo THEN
            part_state := 'future';
        ELSE
            part_state := 'open';
        END IF;
        part_refusal := NULL;
        IF NOT v_first THEN
            part_refusal := 'audit_retention_not_oldest';
        ELSIF part_state <> 'closed' THEN
            part_refusal := 'audit_retention_not_closed';
        ELSIF v_days = 0 OR r.hi + make_interval(days => v_days) > v_now THEN
            part_refusal := 'audit_retention_inside_window';
        ELSE
            EXECUTE format($q$SELECT EXISTS (
                    SELECT 1 FROM @ns@.agent_runs ar
                     WHERE ar.state IN ('PENDING', 'STARTING', 'RUNNING', 'WAITING_FOR_CONFIRMATION')
                       AND EXISTS (SELECT 1 FROM %I.%I p WHERE p.run_id = ar.id))$q$, r.nsp, r.relname) INTO v_live;
            IF v_live THEN
                part_refusal := 'audit_retention_live_run';
            END IF;
        END IF;
        part_eligible := part_refusal IS NULL;
        v_first := false;
        RETURN NEXT;
        IF p_name IS NOT NULL THEN
            RETURN;
        END IF;
    END LOOP;
END;
$body$
$fn$, '@ns@', nsq);

    -- audit_retention_set_policy: the one writer of the policy columns.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_set_policy(p_days integer)
RETURNS TABLE (outcome text, effective_days integer, pending_days integer, pending_effective_at timestamptz)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, @ns@, pg_temp
SET lock_timeout = '5s'
AS $body$
DECLARE
    m      @ns@.audit_partition_meta%ROWTYPE;
    v_now  timestamptz := clock_timestamp();
    v_out  text := 'unchanged';
    v_from integer;
BEGIN
    IF p_days IS NULL OR p_days < 0 OR p_days > 36500 THEN
        RAISE EXCEPTION 'audit_retention_set_policy: p_days must be between 0 (forever) and 36500, got %', p_days;
    END IF;
    -- Chain lock first, then the row (see the header): a policy change is rare, so taking the lock
    -- unconditionally costs one queue position per boot.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    SELECT * INTO m FROM @ns@.audit_partition_meta FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit_retention_set_policy: audit_partition_meta has no row';
    END IF;
    v_from := m.retention_days;
    -- A decrease whose date has passed is the policy now.
    IF m.pending_days IS NOT NULL AND m.pending_effective_at <= v_now THEN
        m.retention_days := m.pending_days;
        m.pending_days := NULL;
        m.pending_effective_at := NULL;
        v_out := 'applied';
    END IF;
    IF p_days = m.retention_days THEN
        -- Re-stating the current value withdraws a pending decrease.
        IF m.pending_days IS NOT NULL THEN
            m.pending_days := NULL;
            m.pending_effective_at := NULL;
            v_out := 'cancelled';
        END IF;
    ELSIF p_days = 0 OR (m.retention_days <> 0 AND p_days > m.retention_days) THEN
        -- An increase (0 is forever) applies at once and clears anything pending.
        m.retention_days := p_days;
        m.pending_days := NULL;
        m.pending_effective_at := NULL;
        v_out := 'applied';
    ELSIF m.pending_days IS DISTINCT FROM p_days THEN
        -- A decrease (0 -> finite included): 30 days from now. The same value again does NOT reach this
        -- branch, so a restart cannot move the date.
        m.pending_days := p_days;
        m.pending_effective_at := v_now + interval '30 days';
        v_out := 'pending';
    END IF;
    IF v_out <> 'unchanged' THEN
        UPDATE @ns@.audit_partition_meta
           SET retention_days = m.retention_days, pending_days = m.pending_days,
               pending_effective_at = m.pending_effective_at, retention_set_at = v_now;
        PERFORM @ns@.audit_append(gen_random_uuid(), v_now, NULL::uuid, 'system', 'wardynd',
            'audit.retention.set', 'audit_partition_meta', 'success', '',
            jsonb_build_object('outcome', v_out, 'requested_days', p_days, 'from_days', v_from,
                'retention_days', m.retention_days, 'pending_days', m.pending_days,
                'pending_effective_at', to_char(m.pending_effective_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')));
    END IF;
    outcome := v_out;
    effective_days := m.retention_days;
    pending_days := m.pending_days;
    pending_effective_at := m.pending_effective_at;
    RETURN NEXT;
END;
$body$
$fn$, '@ns@', nsq);

    -- audit_retention_drop: the attested drop.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_drop(p_partition text, p_digest text, p_actor text)
RETURNS TABLE (dropped_partition text, dropped_rows bigint, dropped_seq_lo bigint, dropped_seq_hi bigint,
               dropped_digest text, dropped_event_seq bigint)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, @ns@, pg_temp
SET lock_timeout = '5s'
AS $body$
DECLARE
    v_parent oid := '@ns@.audit_events'::regclass;
    v_nsp    text;
    v_days   integer;
    e        record;
    v_digest text;
    v_cnt    bigint;
    v_seq_lo bigint;
    v_seq_hi bigint;
    v_rec_lo timestamptz;
    v_rec_hi timestamptz;
    v_tail   text;
    v_event  bigint;
BEGIN
    IF p_actor IS NULL OR p_actor = '' OR length(p_actor) > 320 THEN
        RAISE EXCEPTION 'audit_retention_drop: p_actor must name who is dropping the partition';
    END IF;
    -- The name is used as an identifier below, so it must be a partition of THE audit table.
    SELECT n.nspname INTO v_nsp
      FROM pg_catalog.pg_inherits i
      JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE i.inhparent = v_parent AND c.relname = p_partition;
    IF v_nsp IS NULL THEN
        RAISE EXCEPTION 'audit_retention_drop: % is not a partition of audit_events', p_partition USING ERRCODE = 'WR006';
    END IF;

    -- 1. Refusals, then the scan: all of it outside the chain lock.
    SELECT * INTO e FROM @ns@.audit_retention_partitions(p_partition, false);
    PERFORM @ns@.audit_retention_refuse(e.part_refusal, p_partition);
    v_digest := @ns@.audit_partition_digest(p_partition);
    IF p_digest IS NULL OR v_digest <> lower(p_digest) THEN
        RAISE EXCEPTION 'audit_retention_drop: the digest supplied for % does not match the partition''s own', p_partition
            USING ERRCODE = 'WR005';
    END IF;
    EXECUTE format($q$SELECT count(*), min(seq), max(seq), min(recorded_at), max(recorded_at),
                             (SELECT t.row_hash FROM %1$I.%2$I t ORDER BY t.seq DESC LIMIT 1)
                        FROM %1$I.%2$I$q$, v_nsp, p_partition)
       INTO v_cnt, v_seq_lo, v_seq_hi, v_rec_lo, v_rec_hi, v_tail;

    -- 2. The drop, under the partition lock (db.AuditPartitionLockKey: a concurrent audit_ensure_partitions
    -- rewrites the manifest from what it read, and must not resurrect the entry removed below) and then the
    -- chain lock. The partition is closed, so the scan above still holds; what can have changed is the
    -- catalog (another drop) and the policy, both re-read here. Not taken before the scan: a boot's
    -- audit_ensure_partitions would then queue behind a long scan and give up at its lock_timeout.
    PERFORM pg_advisory_xact_lock(6287397008294629460);
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    SELECT * INTO e FROM @ns@.audit_retention_partitions(p_partition, false);
    IF e.part_name IS NULL THEN
        RAISE EXCEPTION 'audit_retention_drop: % is no longer a partition of audit_events', p_partition USING ERRCODE = 'WR006';
    END IF;
    PERFORM @ns@.audit_retention_refuse(e.part_refusal, p_partition);
    v_days := @ns@.audit_retention_window();

    -- (1) the chained event. It lands in the live partition, after the dropped one's tail.
    SELECT a.seq INTO v_event
      FROM @ns@.audit_append(gen_random_uuid(), clock_timestamp(), NULL::uuid,
            CASE WHEN p_actor = 'wardynd' THEN 'system' ELSE 'human' END, p_actor,
            'audit.retention.partition_dropped', p_partition, 'success', '',
            jsonb_build_object('partition', p_partition, 'row_count', v_cnt, 'seq_lo', v_seq_lo, 'seq_hi', v_seq_hi,
                'recorded_lo', to_char(v_rec_lo AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                'recorded_hi', to_char(v_rec_hi AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                'digest', v_digest, 'retention_days', v_days,
                -- Only an operator who exported and checked the partition attests it; the sweeper's
                -- autodrop is the system actor and is not.
                'attested', p_actor <> 'wardynd')) a;

    -- (2) the anchor. Verify starts from the newest 'drop' anchor with row_count > 0 and requires the first
    -- retained row to chain to tail_row_hash.
    INSERT INTO @ns@.audit_chain_anchors
        (kind, partition_name, seq_lo, seq_hi, recorded_lo, recorded_hi, row_count, digest, tail_row_hash, actor, event_seq)
    VALUES ('drop', p_partition, v_seq_lo, v_seq_hi, v_rec_lo, v_rec_hi, v_cnt, v_digest, v_tail, p_actor, v_event);

    -- (3) the manifest, then DETACH + DROP. Non-concurrent, because a function runs inside a transaction:
    -- a brief ACCESS EXCLUSIVE on the parent with no scan; lock_timeout bounds the wait behind a long reader.
    UPDATE @ns@.audit_partition_meta
       SET manifest = coalesce((SELECT jsonb_agg(t.el ORDER BY t.ord)
                                  FROM jsonb_array_elements(manifest) WITH ORDINALITY AS t(el, ord)
                                 WHERE t.el->>'name' <> p_partition), '[]'::jsonb);
    EXECUTE format('ALTER TABLE @ns@.audit_events DETACH PARTITION %I.%I', v_nsp, p_partition);
    EXECUTE format('DROP TABLE %I.%I', v_nsp, p_partition);

    dropped_partition := p_partition;
    dropped_rows := v_cnt;
    dropped_seq_lo := v_seq_lo;
    dropped_seq_hi := v_seq_hi;
    dropped_digest := v_digest;
    dropped_event_seq := v_event;
    RETURN NEXT;
END;
$body$
$fn$, '@ns@', nsq);

    -- audit_retention_refuse: turns a refusal reason from audit_retention_partitions into its SQLSTATE.
    -- A NULL reason is not a refusal. A helper so the two call sites above cannot disagree. Only the
    -- drop function (the owner's) calls it, so no role is granted it.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_refuse(p_reason text, p_partition text) RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
BEGIN
    IF p_reason IS NULL THEN
        RETURN;
    END IF;
    RAISE EXCEPTION 'audit_retention_drop: % is refused (%)', p_partition, p_reason
        USING ERRCODE = CASE p_reason
            WHEN 'audit_retention_not_oldest'    THEN 'WR001'
            WHEN 'audit_retention_not_closed'    THEN 'WR002'
            WHEN 'audit_retention_inside_window' THEN 'WR003'
            WHEN 'audit_retention_live_run'      THEN 'WR004'
            ELSE 'WR006' END;
END;
$body$
$fn$, '@ns@', nsq);

    -- Function privileges: nobody by default, then exactly the roles that may call audit_append, read from
    -- the catalog and never by name (the owner and PUBLIC are skipped: the owner needs no grant, PUBLIC
    -- gets nothing).
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_window() FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_partitions(text, boolean) FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_set_policy(integer) FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_drop(text, text, text) FROM PUBLIC', '@ns@', nsq);
    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_refuse(text, text) FROM PUBLIC', '@ns@', nsq);
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_window() TO %I', ns, r.rolname);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_partitions(text, boolean) TO %I', ns, r.rolname);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_set_policy(integer) TO %I', ns, r.rolname);
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_drop(text, text, text) TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
