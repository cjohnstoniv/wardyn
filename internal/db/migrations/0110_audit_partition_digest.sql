-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- audit_partition_digest(p_partition): the bounded, canonical digest of one CLOSED audit partition
-- (0.8.6 audit retention; design record docs/design/0.8/0.8.6-ar.md, decision D4 and "Partition
-- lifecycle").
--
-- WHY A FOLD. Concatenating the rows' 64-character hashes into one string (string_agg) reaches
-- Postgres's 1 GB field limit near sixteen million rows, so such a partition could never be dropped,
-- and it returns NULL for an empty one. This is a streaming fold through a cursor, one row in memory:
--
--   d_0 = sha256(manifest_header)
--   d_i = sha256(d_{i-1} || row_hash_i)           in seq order
--
-- Every value is HEX TEXT, the way the chain itself is written (audit_row_hash hashes
-- coalesce(prev, '') || json, both text), and the digest is the 64-character hex of the last step. A
-- fold over hex text here and over raw bytes in the export would give two digests that each "work";
-- there is one definition, and store.PartitionDigest (Go) is its other implementation, pinned to this
-- function by a test.
--
-- manifest_header binds the partition name, the seq range, the recorded_at range and the row count,
-- encoded the way audit_row_hash encodes: a JSON array of strings (so no separator can be smuggled from
-- one field into the next), times as integer microseconds since the epoch (locale- and
-- TimeZone-independent). The ranges are the min and max of the rows actually held, not the partition's
-- bounds, so a raw archive can recompute them from its own rows. An empty partition has empty strings
-- for the ranges and a count of 0; its digest is d_0.
--
-- A row from before the chain began (NULL row_hash, 0047's legacy rows) folds
-- audit_row_hash(NULL, ...), the hash it would have had.
--
-- CLOSED ONLY. A partition is closed once the high-water recorded_at is at or past its upper bound: no
-- later append can route into it (audit_append sets recorded_at in chain order), so its digest cannot
-- change after it is taken. An open partition is refused. The fold takes no lock: a closed partition
-- is immutable, and the drop function (ar-l1.3) holds the chain lock only for the drop itself.
--
-- HARDENING is 0058's, as in 0108: every name qualified with the ACTUAL schema discovered from the
-- catalog, the pinned search_path ends with pg_temp, the partition NAME is validated against the
-- parent's pg_inherits before it is used as an identifier, EXECUTE is revoked from PUBLIC and granted to
-- exactly the roles that hold it on audit_append.

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
CREATE FUNCTION @ns@.audit_partition_digest(p_partition text) RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, @ns@, pg_temp
AS $body$
DECLARE
    v_parent  oid := '@ns@.audit_events'::regclass;
    v_oid     oid;
    v_ns      text;
    v_bound   text;
    v_upper   timestamptz;
    v_hw      timestamptz;
    v_cnt     bigint;
    v_seq_lo  bigint;
    v_seq_hi  bigint;
    v_rec_lo  bigint;
    v_rec_hi  bigint;
    d         text;
    h         text;
    r         record;
BEGIN
    -- The name is used as an identifier below, so it must be a partition of THE audit table.
    SELECT c.oid, n.nspname, pg_catalog.pg_get_expr(c.relpartbound, c.oid) INTO v_oid, v_ns, v_bound
      FROM pg_catalog.pg_inherits i
      JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE i.inhparent = v_parent
       AND c.relname = p_partition
       AND c.relnamespace = (SELECT p.relnamespace FROM pg_catalog.pg_class p WHERE p.oid = v_parent);
    IF v_oid IS NULL THEN
        RAISE EXCEPTION 'audit_partition_digest: % is not a partition of audit_events', p_partition;
    END IF;

    -- Closed: the high-water recorded_at has reached the partition's upper bound. The bound's text is
    -- rendered and parsed in this one session, so the offset it carries round-trips whatever the
    -- TimeZone. A partition with no upper bound (MAXVALUE) is never closed.
    v_upper := (regexp_match(v_bound, 'TO \(''([^'']+)''\)'))[1]::timestamptz;
    SELECT m.hw_recorded_at INTO v_hw FROM @ns@.audit_partition_meta m;
    IF v_upper IS NULL OR v_hw IS NULL OR v_hw < v_upper THEN
        RAISE EXCEPTION 'audit_partition_digest: % is still open (new rows can be appended to it); only a closed partition has a digest', p_partition;
    END IF;

    EXECUTE format($q$SELECT count(*), min(seq), max(seq),
                             (extract(epoch FROM min(recorded_at)) * 1000000)::bigint,
                             (extract(epoch FROM max(recorded_at)) * 1000000)::bigint
                        FROM %I.%I$q$, v_ns, p_partition)
       INTO v_cnt, v_seq_lo, v_seq_hi, v_rec_lo, v_rec_hi;

    d := encode(sha256(convert_to(jsonb_build_array(
            p_partition,
            coalesce(v_seq_lo::text, ''), coalesce(v_seq_hi::text, ''),
            coalesce(v_rec_lo::text, ''), coalesce(v_rec_hi::text, ''),
            v_cnt::text)::text, 'UTF8')), 'hex');

    -- FOR ... IN EXECUTE reads through a cursor, ten rows at a time: memory stays constant however
    -- many rows the partition holds.
    FOR r IN EXECUTE format($q$SELECT id, "time", run_id, actor_type, actor, action, target, outcome,
                                      source_ip, data, row_hash
                                 FROM %I.%I ORDER BY seq$q$, v_ns, p_partition)
    LOOP
        h := coalesce(r.row_hash, @ns@.audit_row_hash(NULL, r.id, r."time", r.run_id, r.actor_type,
                                                       r.actor, r.action, r.target, r.outcome,
                                                       r.source_ip, r.data));
        d := encode(sha256(convert_to(d || h, 'UTF8')), 'hex');
    END LOOP;
    RETURN d;
END;
$body$
$fn$, '@ns@', nsq);

    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_partition_digest(text) FROM PUBLIC', '@ns@', nsq);

    -- The roles that may call audit_append may call this: read from the catalog, never by name, and
    -- the owner and PUBLIC are skipped (the owner needs no grant; PUBLIC gets nothing).
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_partition_digest(text) TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
