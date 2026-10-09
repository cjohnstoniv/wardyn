-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- audit_ensure_partitions keeps the months ahead of the month the chain POINTS AT, not only of the clock.
--
-- audit_append (0111) pins recorded_at to greatest(clock_timestamp(), hw_recorded_at, cutover), but 0111's
-- audit_ensure_partitions computed its target from clock_timestamp() alone. After one append under a database
-- clock stepped forward past every created partition (about 13 months), the high-water mark stayed there
-- once the clock was corrected: every audit_append failed with "no partition of relation audit_events found
-- for row", audit_ensure_partitions created nothing, the boot canary refused every boot, and only hand-run
-- DDL by the database owner recovered it. The target is now taken from greatest(clock, high-water mark,
-- cutover), the same expression audit_append uses, so a partition always exists for whatever the chain
-- already points at, and the next boot (which runs audit_ensure_partitions before the canary) and the daily
-- sweeper heal it. The 1..24 bound and everything else are 0111's.
--
-- Same hardening and conventions as 0123: DROP FUNCTION IF EXISTS then CREATE (idempotent: the migrator's
-- replay tests re-apply the newest migration), every name qualified with the schema discovered from the
-- catalog, and EXECUTE revoked from PUBLIC and granted to exactly the roles that hold it on audit_append
-- (the DROP discards the old grants).

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

    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_ensure_partitions(integer)', '@ns@', nsq);

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
    -- The month the chain already points at (never earlier than the clock's) plus p_months after it, in UTC.
    -- audit_append pins recorded_at to greatest(clock, high-water mark, cutover), so after the database clock
    -- stepped forward and back, partitions must exist for the high-water mark, not only for now().
    target := (date_trunc('month', greatest(clock_timestamp(), m.hw_recorded_at, m.cutover) AT TIME ZONE 'UTC') + make_interval(months => p_months + 1)) AT TIME ZONE 'UTC';
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

    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_ensure_partitions(integer) FROM PUBLIC', '@ns@', nsq);
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_ensure_partitions(integer) TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
