-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A Self-Hosted runner pool belongs to one person (0.9): the person creates it, names it, sets its
-- limits and puts their own claimed runners in it, and nobody else may see or change it. owner is
-- that person's subject, the same identity runners.owner holds. A Remote Provided pool has no owner:
-- administrators keep it. A tombstoned Self-Hosted pool may lose its owner to the person's erasure.
--
-- The organisation no longer has a Self-Hosted default: each person's own default names their own pool,
-- and an organisation preference for Self-Hosted is cleared to none, never switched to Remote Provided.
-- With no organisation preference, a launch that names no pool, from a person with no preference of
-- their own, is refused runner_pool_required until the person or an administrator sets one.
--
-- Clean break: a Self-Hosted pool created before this migration has no owner, so it is removed, with
-- its members, the organisation and personal default slots that name it, and any run still waiting for
-- it (ended KILLED, runner_pool_removed, as a pool delete ends one, but with no run.kill audit row: a
-- migration writes no audit). A pool a run is bound to stays as a tombstone so the run keeps its
-- history; every other one is deleted.
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS owner TEXT CHECK (owner IS NULL OR owner <> '');

UPDATE agent_runs SET state = 'KILLED', updated_at = now(), ended_at = now(),
       status_detail = 'queue: runner_pool_removed', failure_hint = 'The pool this run was waiting for was removed.'
WHERE state = 'QUEUED' AND id IN (SELECT run_id FROM run_queue WHERE pool_id IN (SELECT id FROM runner_pools WHERE hosting_type = 'self_hosted' AND owner IS NULL));
DELETE FROM run_queue WHERE pool_id IN (SELECT id FROM runner_pools WHERE hosting_type = 'self_hosted' AND owner IS NULL);
DELETE FROM runner_pool_members WHERE pool_id IN (SELECT id FROM runner_pools WHERE hosting_type = 'self_hosted' AND owner IS NULL);
DELETE FROM runner_pool_use_policies WHERE pool_id IN (SELECT id FROM runner_pools WHERE hosting_type = 'self_hosted' AND owner IS NULL);
UPDATE runner_pool_org_defaults SET self_hosted = NULL, updated_by = 'system', updated_at = now() WHERE self_hosted IS NOT NULL;
UPDATE runner_pool_org_defaults SET preferred_hosting = '', updated_by = 'system', updated_at = now() WHERE preferred_hosting = 'self_hosted';
UPDATE principal_prefs SET value = value - 'self_hosted', updated_at = now()
WHERE key = 'runner_pool_defaults.v1' AND value->>'self_hosted' IN (SELECT id::text FROM runner_pools WHERE hosting_type = 'self_hosted' AND owner IS NULL);
DELETE FROM runner_pools p WHERE p.hosting_type = 'self_hosted' AND p.owner IS NULL
    AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.runner_pool_id = p.id);
UPDATE runner_pools SET state = 'deleted', revision = revision + 1, updated_at = now()
WHERE hosting_type = 'self_hosted' AND owner IS NULL AND state <> 'deleted';

ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_owner_by_hosting;
ALTER TABLE runner_pools ADD CONSTRAINT runner_pools_owner_by_hosting CHECK (
    (hosting_type = 'remote_provided' AND owner IS NULL)
    OR (hosting_type = 'self_hosted' AND (owner IS NOT NULL OR state = 'deleted')));

ALTER TABLE runner_pool_org_defaults DROP CONSTRAINT IF EXISTS runner_pool_org_defaults_no_self_hosted;
ALTER TABLE runner_pool_org_defaults ADD CONSTRAINT runner_pool_org_defaults_no_self_hosted CHECK (self_hosted IS NULL);

-- Names are unique among one owner's live pools and among the organisation's (owner NULL), so one
-- person's name never tells another that a pool exists.
DROP INDEX IF EXISTS runner_pools_live_name_idx;
CREATE UNIQUE INDEX IF NOT EXISTS runner_pools_live_owner_name_idx ON runner_pools (COALESCE(owner, ''), lower(name)) WHERE state <> 'deleted';
CREATE INDEX IF NOT EXISTS runner_pools_owner_idx ON runner_pools (owner) WHERE owner IS NOT NULL;
