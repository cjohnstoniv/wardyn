-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Audit row hash format 2: a row hashed with a per-row random salt that only the store, the raw partition
-- export and a receipt of an unprojected row carry. Every row audit_append writes from here on is format 2.
-- Rows written before this migration keep format 1 and are never re-hashed; both formats verify.
--
--   hash_format  1 (the default, every existing row) or 2.
--   hash_salt    a fresh 64-hex salt, made inside audit_append, never taken from a caller. NULL on a format 1 row.
--   claim_salt   a federated device's own salt for the claim it pushed with the row, kept only so that claim
--                can be re-checked from the stored row. NULL on every other row. Never exported.
--
-- audit_append keeps its signature, so no caller changes. The chain function and its trigger are re-created
-- in the next migration, which is the one a boot-time restore of the trigger replays last. Idempotent.

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

    -- The partitioned parent propagates all three columns to every partition; new partitions are made
    -- LIKE the parent INCLUDING DEFAULTS and follow. Existing rows read 1 from the default, with no rewrite.
    EXECUTE replace('ALTER TABLE @ns@.audit_events
        ADD COLUMN IF NOT EXISTS hash_format smallint NOT NULL DEFAULT 1,
        ADD COLUMN IF NOT EXISTS hash_salt   text,
        ADD COLUMN IF NOT EXISTS claim_salt  text', '@ns@', nsq);

    -- Format 2: format 1's body (0047), with the array's first two elements 'v2' and the salt.
    EXECUTE replace($fn$
CREATE OR REPLACE FUNCTION @ns@.audit_row_hash_v2(
    prev          TEXT,
    salt          TEXT,
    p_id          UUID,
    p_time        TIMESTAMPTZ,
    p_run_id      UUID,
    p_actor_type  TEXT,
    p_actor       TEXT,
    p_action      TEXT,
    p_target      TEXT,
    p_outcome     TEXT,
    p_source_ip   TEXT,
    p_data        JSONB
) RETURNS TEXT LANGUAGE sql STABLE AS $body$
    SELECT encode(sha256(convert_to(
        coalesce(prev, '') ||
        jsonb_build_array(
            'v2',
            salt,
            p_id::text,
            (extract(epoch FROM p_time) * 1000000)::bigint::text,
            coalesce(p_run_id::text, ''),
            p_actor_type, p_actor, p_action, p_target, p_outcome, p_source_ip,
            p_data
        )::text, 'UTF8')), 'hex')
$body$
$fn$, '@ns@', nsq);

    -- The format picks the hash: 1 is the original, 2 is salted, anything else is NULL, which a sweep reads
    -- as a mismatch at that seq and never as a pass.
    EXECUTE replace($fn$
CREATE OR REPLACE FUNCTION @ns@.audit_row_hash_any(
    fmt           SMALLINT,
    salt          TEXT,
    prev          TEXT,
    p_id          UUID,
    p_time        TIMESTAMPTZ,
    p_run_id      UUID,
    p_actor_type  TEXT,
    p_actor       TEXT,
    p_action      TEXT,
    p_target      TEXT,
    p_outcome     TEXT,
    p_source_ip   TEXT,
    p_data        JSONB
) RETURNS TEXT LANGUAGE sql STABLE AS $body$
    SELECT CASE fmt
        WHEN 1 THEN @ns@.audit_row_hash(prev, p_id, p_time, p_run_id, p_actor_type, p_actor, p_action,
                                       p_target, p_outcome, p_source_ip, p_data)
        WHEN 2 THEN @ns@.audit_row_hash_v2(prev, salt, p_id, p_time, p_run_id, p_actor_type, p_actor, p_action,
                                          p_target, p_outcome, p_source_ip, p_data)
    END
$body$
$fn$, '@ns@', nsq);

    -- audit_append: 0111's body, with the format and a fresh salt set in its INSERT, and a device's claim
    -- salt taken from the transaction-local setting wardyn.audit_claim_salt and cleared after the row.
    -- CREATE OR REPLACE replaces the function's SET clauses, so the path 0285 pinned is stated again.
    EXECUTE replace($fn$
CREATE OR REPLACE FUNCTION @ns@.audit_append(
    p_id uuid, p_time timestamptz, p_run_id uuid, p_actor_type text, p_actor text,
    p_action text, p_target text, p_outcome text, p_source_ip text, p_data jsonb)
RETURNS TABLE (seq bigint, recorded_at timestamptz, prev_hash text, row_hash text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
DECLARE
    m       @ns@.audit_partition_meta%ROWTYPE;
    v_seq   bigint;
    v_at    timestamptz;
    v_prev  text;
    v_hash  text;
    v_claim text := NULLIF(pg_catalog.current_setting('wardyn.audit_claim_salt', true), '');
BEGIN
    -- db.AuditChainLockKey: position, timestamp and chain link are decided under ONE lock.
    PERFORM pg_advisory_xact_lock(6287397008294758465);
    SELECT * INTO m FROM @ns@.audit_partition_meta;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'audit_append: audit_partition_meta has no row';
    END IF;
    IF v_claim IS NOT NULL AND v_claim !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'audit_append: wardyn.audit_claim_salt is not 64 lowercase hex' USING ERRCODE = '22023';
    END IF;
    v_seq := nextval('@ns@.audit_events_seq');
    -- Read from the high-water mark, never from the head row, so a stray row cannot drag the
    -- clock; greatest() keeps recorded_at monotonic under a clock step backwards.
    v_at := greatest(clock_timestamp(), m.hw_recorded_at, m.cutover);
    PERFORM pg_catalog.set_config('wardyn.audit_seq', v_seq::text, true);
    INSERT INTO @ns@.audit_events
        (seq, id, "time", run_id, actor_type, actor, action, target, outcome, source_ip, data, recorded_at,
         hash_format, hash_salt, claim_salt)
    VALUES
        (v_seq, p_id, p_time, p_run_id, p_actor_type, p_actor, p_action, p_target, p_outcome, p_source_ip, p_data, v_at,
         2, replace(gen_random_uuid()::text, '-', '') || replace(gen_random_uuid()::text, '-', ''), v_claim)
    RETURNING audit_events.prev_hash, audit_events.row_hash INTO v_prev, v_hash;
    PERFORM pg_catalog.set_config('wardyn.audit_claim_salt', '', true);
    UPDATE @ns@.audit_partition_meta
       SET hw_seq = v_seq, hw_recorded_at = v_at, hw_row_hash = v_hash;
    RETURN QUERY SELECT v_seq, v_at, v_prev, v_hash;
END;
$body$
$fn$, '@ns@', nsq);
END
$mig$;
