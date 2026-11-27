-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Windows VM sandboxes (machine_kind windows_vm), and the licence fence that keeps them inside the organisation's
-- pools. agent_runs.machine_kind admits the new kind beside '', 'linux_vm' and 'macos_vm' (WIDENS the CHECK and
-- changes no row; re-runnable, the constraint is dropped if present first).
--
-- Three triggers on agent_runs hold the fence, so no create path, whatever binary sends the SQL, starts a
-- Windows VM without a seat, and only the run's end frees one:
--
-- - windows_licence_take, AFTER INSERT of a windows_vm run, in the same statement: it refuses a run inserted
--   already ended (windows_vm_inserted_ended), and refuses the run unless the stored site config's
--   features.windows_vm.enabled is the JSON boolean true (windows_vms_off; no row, no key, a string or false all
--   refuse). It then reads the active image whose ref is the run's image (windows_image_unregistered), its pool,
--   which must be active (windows_licence_pool_inactive), and the run creator's holder row in that pool
--   (windows_licence_not_holder), each FOR SHARE so a retire, a pool delete or a holder removal waits for the
--   take or sees its lease, and inserts the lease. A holder with a live seat in the pool already is refused
--   (windows_licence_in_use).
-- - windows_licence_release, AFTER UPDATE of a windows_vm run's state: a terminal state (COMPLETED, FAILED,
--   KILLED, STOPPED, ARCHIVED) deletes its lease. A lost run keeps its seat, since its VM may still run.
-- - zz_windows_vm_fence, BEFORE UPDATE of a run that is or would be windows_vm, named to fire after every other
--   BEFORE trigger so it judges the row as they left it: it refuses a change of machine_kind into or out of
--   windows_vm (windows_vm_kind_fixed), of created_by (windows_vm_holder_fixed) or of image
--   (windows_vm_image_fixed; the same value again passes), and a move from a terminal state back to a live one
--   (windows_vm_ended).
--
-- Each refusal is SQLSTATE 23514 naming its constraint. No trigger has a column list: one fires only when a
-- listed column is in the statement's SET list, so a change another trigger made would slip past it. Each WHEN
-- clause keeps the cost to Windows rows.
--
-- The functions are written as 0293 writes run_support_grants_end_with_run(): the schema read from agent_runs,
-- every table in it, every function in pg_catalog, and a path of pg_catalog then pg_temp, so a namespaced install
-- finds its own tables and a definer function never resolves a name through a schema other roles may write. They
-- run as their owner, the migrator, so the app role, which only reads windows_licence_leases, never takes or
-- frees a seat except by inserting or ending a run.
--
-- The take is the one SQL reader of a stored features switch. It reads the switch without asking whether this
-- release can turn the feature on, which is right only while the windows_vm key is always available: a release
-- that makes the key unavailable changes this trigger in the same commit.
ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_machine_kind,
    ADD CONSTRAINT agent_runs_machine_kind CHECK (machine_kind IN ('', 'linux_vm', 'macos_vm', 'windows_vm'));

DO $mig$
DECLARE
    ns text;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'agent_runs'::regclass;

    EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.windows_licence_take() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
DECLARE
    v_image  uuid;
    v_pool   uuid;
    v_holder uuid;
BEGIN
    IF NEW.state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED') THEN
        RAISE EXCEPTION 'a Windows VM run cannot be inserted already ended'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vm_inserted_ended';
    END IF;
    IF (SELECT (c.config->'features'->'windows_vm'->'enabled') = 'true'::jsonb
          FROM %1$I.site_config c WHERE c.singleton) IS NOT TRUE THEN
        RAISE EXCEPTION 'Windows VM sandboxes are turned off in this organisation'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vms_off';
    END IF;
    SELECT i.id, i.licence_pool_id INTO v_image, v_pool
      FROM %1$I.windows_images i
     WHERE i.ref = NEW.image AND i.state = 'active'
       FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the run''s image is not a registered Windows image'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_image_unregistered';
    END IF;
    PERFORM 1
       FROM %1$I.windows_licence_pools p
      WHERE p.id = v_pool AND p.state = 'active'
        FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the Windows image''s licence pool is not active'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_licence_pool_inactive';
    END IF;
    SELECT h.id INTO v_holder
      FROM %1$I.windows_licence_holders h
     WHERE h.pool_id = v_pool AND pg_catalog.lower(h.subject) = pg_catalog.lower(pg_catalog.btrim(NEW.created_by))
       FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the run''s creator holds no seat in the Windows image''s licence pool'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_licence_not_holder';
    END IF;
    INSERT INTO %1$I.windows_licence_leases (run_id, pool_id, holder_id, image_id)
    VALUES (NEW.id, v_pool, v_holder, v_image)
    ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the run''s creator already has a live Windows VM in this licence pool'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_licence_in_use';
    END IF;
    RETURN NULL;
END;
$body$
$fn$, ns);

    EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.windows_licence_release() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
BEGIN
    IF NEW.state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED') THEN
        DELETE FROM %1$I.windows_licence_leases WHERE run_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$body$
$fn$, ns);

    EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.windows_vm_fence() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
BEGIN
    IF OLD.machine_kind IS DISTINCT FROM NEW.machine_kind THEN
        RAISE EXCEPTION 'a run cannot move into or out of the windows_vm kind'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vm_kind_fixed';
    END IF;
    IF OLD.created_by IS DISTINCT FROM NEW.created_by THEN
        RAISE EXCEPTION 'a Windows VM run''s creator holds its seat and cannot change'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vm_holder_fixed';
    END IF;
    IF OLD.image IS DISTINCT FROM NEW.image THEN
        RAISE EXCEPTION 'a Windows VM run''s image names its licence pool and cannot change'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vm_image_fixed';
    END IF;
    IF OLD.state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED')
       AND NEW.state NOT IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED') THEN
        RAISE EXCEPTION 'an ended Windows VM run cannot come back'
            USING ERRCODE = '23514', CONSTRAINT = 'windows_vm_ended';
    END IF;
    RETURN NEW;
END;
$body$
$fn$, ns);

    EXECUTE pg_catalog.format('DROP TRIGGER IF EXISTS windows_licence_take ON %1$I.agent_runs', ns);
    EXECUTE pg_catalog.format($tr$
CREATE TRIGGER windows_licence_take
    AFTER INSERT ON %1$I.agent_runs
    FOR EACH ROW
    WHEN (NEW.machine_kind = 'windows_vm')
    EXECUTE FUNCTION %1$I.windows_licence_take()
$tr$, ns);

    EXECUTE pg_catalog.format('DROP TRIGGER IF EXISTS windows_licence_release ON %1$I.agent_runs', ns);
    EXECUTE pg_catalog.format($tr$
CREATE TRIGGER windows_licence_release
    AFTER UPDATE ON %1$I.agent_runs
    FOR EACH ROW
    WHEN (OLD.machine_kind = 'windows_vm' AND OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION %1$I.windows_licence_release()
$tr$, ns);

    EXECUTE pg_catalog.format('DROP TRIGGER IF EXISTS zz_windows_vm_fence ON %1$I.agent_runs', ns);
    EXECUTE pg_catalog.format($tr$
CREATE TRIGGER zz_windows_vm_fence
    BEFORE UPDATE ON %1$I.agent_runs
    FOR EACH ROW
    WHEN (OLD.machine_kind = 'windows_vm' OR NEW.machine_kind = 'windows_vm')
    EXECUTE FUNCTION %1$I.windows_vm_fence()
$tr$, ns);
END
$mig$;
