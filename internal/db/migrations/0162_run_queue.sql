-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The pool run queue (0.9, docs/design/0.9/pool-queue.md): a launch into a runner
-- pool at its concurrent-run cap waits as a QUEUED run instead of failing.
--
--   state QUEUED    non-terminal, before PENDING. A QUEUED run holds no slot, no
--                   identity and no sandbox; spiffe_id stays NOT NULL and is ''
--                   until the run starts.
--   admitted_at     written by promotion only (QUEUED -> PENDING). NULL for a run
--                   that never waited, so every legacy row is unchanged. The
--                   undispatched and max-age reapers age a run from
--                   COALESCE(admitted_at, created_at), so a long wait is not
--                   counted against a run the moment it starts.
--   run_queue       one row exactly while its run is QUEUED. Every exit from QUEUED
--                   (promotion, kill, timeout, suspension, pool removal) deletes the
--                   row in the same transaction as the state change, so the per-person
--                   count, the position and the promoter's scan read this table alone.
--                   authority and launch hold the owner's enqueue-time session facts
--                   and the decoded request: personal data that goes with the row.
--                   pool_id is ON DELETE RESTRICT, so a pool with waiting runs cannot
--                   be hard-deleted past them (waiting runs are cancelled first).
--
-- The state CHECK and the two state lists below (the watcher sweep index and the
-- audit-retention live-run check) are kept equal to types.NonTerminalRunStates by
-- TestNonTerminalRunStates_MatchesTypes and the audit-retention pins.

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_state_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_state_check CHECK (state IN (
    'QUEUED',
    'PENDING',
    'STARTING',
    'RUNNING',
    'WAITING_FOR_CONFIRMATION',
    'COMPLETED',
    'STOPPED',
    'ARCHIVED',
    'FAILED',
    'KILLED'
));

ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS admitted_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS run_queue (
    run_id      UUID PRIMARY KEY REFERENCES agent_runs (id) ON DELETE CASCADE,
    pool_id     UUID NOT NULL REFERENCES runner_pools (id) ON DELETE RESTRICT,
    owner       TEXT NOT NULL,
    queued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline_at TIMESTAMPTZ NOT NULL,
    policy_ref  JSONB NOT NULL DEFAULT '{}'::jsonb,
    authority   JSONB NOT NULL DEFAULT '{}'::jsonb,
    launch      JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS run_queue_pool_order_idx ON run_queue (pool_id, queued_at, run_id);
CREATE INDEX IF NOT EXISTS run_queue_deadline_idx ON run_queue (deadline_at);
CREATE INDEX IF NOT EXISTS run_queue_pool_owner_idx ON run_queue (pool_id, owner);

DROP INDEX IF EXISTS agent_runs_watcher_sweep_idx;
CREATE INDEX IF NOT EXISTS agent_runs_watcher_sweep_idx
    ON agent_runs (watcher_heartbeat)
    WHERE state IN ('QUEUED','PENDING','STARTING','RUNNING','WAITING_FOR_CONFIRMATION');

-- audit_retention_partitions: 0123's function with QUEUED in the live-run set (a
-- waiting run is live and cancellable, so its audit rows keep their partition).
-- 0146 re-created only audit_retention_drop, which asks this function, so this is
-- the newest definition. Re-created the way 0123 and 0146 do: DROP FUNCTION IF
-- EXISTS then CREATE (never CREATE OR REPLACE), schema from the catalog, EXECUTE
-- revoked from PUBLIC and granted to exactly the roles that hold it on audit_append.
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

    EXECUTE replace('DROP FUNCTION IF EXISTS @ns@.audit_retention_partitions(text, boolean)', '@ns@', nsq);

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
                     WHERE ar.state IN ('QUEUED', 'PENDING', 'STARTING', 'RUNNING', 'WAITING_FOR_CONFIRMATION')
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

    EXECUTE replace('REVOKE ALL ON FUNCTION @ns@.audit_retention_partitions(text, boolean) FROM PUBLIC', '@ns@', nsq);
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION %I.audit_retention_partitions(text, boolean) TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
