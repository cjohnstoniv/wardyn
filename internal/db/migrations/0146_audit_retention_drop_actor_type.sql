-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- audit_retention_drop records WHO dropped the partition as well as the name: a new p_actor_type
-- (human or system, default NULL = the 0123 rule).
--
-- 0123 typed the chained audit.retention.partition_dropped event 'system' only for the sweeper's name
-- ('wardynd') and 'human' for every other actor, so a drop made with the shared admin token (which is not a
-- person) was recorded as a human. The API now passes the caller's own actor type; the sweeper stays the
-- system actor whatever is passed, and attestation still follows the actor's name (everyone but the sweeper
-- attests), so nothing about attested changes.
--
-- Same hardening and conventions as 0123 and 0138: DROP FUNCTION IF EXISTS then CREATE (idempotent: the
-- migrator's replay tests re-apply the newest migration; the three-argument 0123 form is dropped), every
-- name qualified with the schema discovered from the catalog, and EXECUTE revoked from PUBLIC and granted to
-- exactly the roles that hold it on audit_append (the DROP discards the old grants).

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

    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_drop(text, text, text)', '@ns@', nsq);
    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_drop(text, text, text, text)', '@ns@', nsq);

    -- audit_retention_drop: the attested drop.
    EXECUTE replace($fn$
CREATE FUNCTION @ns@.audit_retention_drop(p_partition text, p_digest text, p_actor text, p_actor_type text DEFAULT NULL)
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
    v_type   text;
BEGIN
    IF p_actor IS NULL OR p_actor = '' OR length(p_actor) > 320 THEN
        RAISE EXCEPTION 'audit_retention_drop: p_actor must name who is dropping the partition';
    END IF;
    IF p_actor_type IS NOT NULL AND p_actor_type NOT IN ('human', 'system') THEN
        RAISE EXCEPTION 'audit_retention_drop: p_actor_type must be human or system';
    END IF;
    -- The sweeper (wardynd) is always the system actor. Anyone else is a human unless the caller says
    -- otherwise: the shared admin token is not a person, so it passes 'system'. Attestation below follows
    -- the actor's name, not its type, so an admin-token drop is still attested.
    v_type := CASE WHEN p_actor = 'wardynd' THEN 'system' ELSE coalesce(p_actor_type, 'human') END;
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
            v_type, p_actor,
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

    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_drop(text, text, text, text) FROM PUBLIC', '@ns@', nsq);
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_drop(text, text, text, text) TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
