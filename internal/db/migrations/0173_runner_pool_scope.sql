-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0
-- Organization pools and user pools (0.9). A pool's scope replaces its hosting type: an organization
-- pool (no owner, administrators keep it) holds remote-hosted runners only, and a user pool (one
-- person's own) holds that person's own claimed self-hosted runners and the remote-hosted runners an
-- administrator made available to people's pools. Which kind a member may be is the writer's check,
-- under the pool row lock, as before.
--
-- A remote-hosted runner is the server's own Docker host or Kubernetes cluster. It now has a row,
-- keyed by its configured substrate name, so it can carry a display name and its "Available to people's
-- pools" policy: off (the default), everyone, or listed people, groups and roles. people_subjects is
-- set exactly when the policy is listed; a leaver purge may empty that list, and an empty list admits
-- nobody. An id never parses as a UUID, so a runner id and a remote runner id never collide.
--
-- Clean break: hosting_type becomes scope (remote_provided -> organization, self_hosted -> user) and is
-- dropped; the member column executor_id becomes remote_runner_id with a foreign key to its row; the
-- organisation defaults keep one pool (the former remote slot) and the default barrier, and lose the
-- preferred hosting type; each person's runner_pool_defaults.v1 document becomes runner_pool_default.v2
-- holding the one pool it would have resolved to (the slot of the person's preferred hosting type,
-- else the organisation's, else the only slot set), and every v1 document is deleted. A live
-- organisation pool still called "Remote Provided" is renamed "Organization" unless that name is taken.
-- A held executor attach or detach waiting for a second human names the remote runner instead; it was
-- reviewed against the old state, so it goes stale at its decision.
-- It runs once: a schema whose members already name remote_runner_id has nothing left to convert.
DO $$
BEGIN
IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()
               AND table_name = 'runner_pool_members' AND column_name = 'executor_id') THEN
    RETURN;
END IF;

CREATE TABLE IF NOT EXISTS remote_runners (
    id              TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_.-]{0,62}$'
                        AND id !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    name            TEXT NOT NULL CHECK (name <> '' AND char_length(name) <= 64),
    people_pools    TEXT NOT NULL DEFAULT 'off' CHECK (people_pools IN ('off', 'everyone', 'listed')),
    people_subjects JSONB CHECK (people_subjects IS NULL OR jsonb_typeof(people_subjects) = 'array'),
    revision        BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    updated_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT remote_runners_people_subjects_iff_listed CHECK ((people_pools = 'listed') = (people_subjects IS NOT NULL))
);
INSERT INTO remote_runners (id, name, updated_by)
SELECT DISTINCT executor_id,
       CASE executor_id WHEN 'docker' THEN 'Docker host' WHEN 'k8s' THEN 'Kubernetes cluster' ELSE executor_id END, 'system'
FROM runner_pool_members WHERE executor_id IS NOT NULL
ON CONFLICT (id) DO NOTHING;

ALTER TABLE runner_pool_members RENAME COLUMN executor_id TO remote_runner_id;
ALTER INDEX IF EXISTS runner_pool_members_executor_idx RENAME TO runner_pool_members_remote_runner_idx;
ALTER TABLE runner_pool_members ADD CONSTRAINT runner_pool_members_remote_runner_fk
    FOREIGN KEY (remote_runner_id) REFERENCES remote_runners (id);
ALTER TABLE runner_pool_members ADD COLUMN IF NOT EXISTS kind TEXT GENERATED ALWAYS AS
    (CASE WHEN runner_id IS NOT NULL THEN 'self_hosted' ELSE 'remote_hosted' END) STORED;
CREATE INDEX IF NOT EXISTS runner_pool_members_by_remote_runner_idx ON runner_pool_members (remote_runner_id) WHERE remote_runner_id IS NOT NULL;

ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS scope TEXT;
UPDATE runner_pools SET scope = CASE hosting_type WHEN 'self_hosted' THEN 'user' ELSE 'organization' END;
ALTER TABLE runner_pools ALTER COLUMN scope SET NOT NULL;
ALTER TABLE runner_pools ADD CONSTRAINT runner_pools_scope_valid CHECK (scope IN ('organization', 'user'));
ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_owner_by_hosting;
ALTER TABLE runner_pools ADD CONSTRAINT runner_pools_owner_by_scope CHECK (
    (scope = 'organization' AND owner IS NULL)
    OR (scope = 'user' AND (owner IS NOT NULL OR state = 'deleted')));
ALTER TABLE runner_pools DROP COLUMN hosting_type;
UPDATE runner_pools p SET name = 'Organization', updated_at = now()
WHERE p.owner IS NULL AND p.state <> 'deleted' AND p.name = 'Remote Provided'
  AND NOT EXISTS (SELECT 1 FROM runner_pools o WHERE o.owner IS NULL AND o.state <> 'deleted' AND lower(o.name) = 'organization');

-- The personal documents read the organisation's preference before the column goes.
INSERT INTO principal_prefs (principal, key, value, updated_at)
SELECT v1.principal, 'runner_pool_default.v2', jsonb_build_object('pool_id', v1.pool), now()
FROM (
    SELECT pp.principal,
           CASE COALESCE(NULLIF(pp.value->>'preferred_hosting', ''), NULLIF(od.preferred_hosting, ''))
               WHEN 'remote_provided' THEN pp.value->>'remote_provided'
               WHEN 'self_hosted' THEN pp.value->>'self_hosted'
               ELSE CASE WHEN (pp.value ? 'remote_provided') <> (pp.value ? 'self_hosted')
                         THEN COALESCE(pp.value->>'remote_provided', pp.value->>'self_hosted') END
           END AS pool
    FROM principal_prefs pp LEFT JOIN runner_pool_org_defaults od ON TRUE
    WHERE pp.key = 'runner_pool_defaults.v1'
) v1
WHERE v1.pool IS NOT NULL
ON CONFLICT (principal, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
DELETE FROM principal_prefs WHERE key = 'runner_pool_defaults.v1';

ALTER TABLE runner_pool_org_defaults DROP CONSTRAINT IF EXISTS runner_pool_org_defaults_no_self_hosted;
ALTER TABLE runner_pool_org_defaults DROP COLUMN self_hosted;
ALTER TABLE runner_pool_org_defaults DROP COLUMN preferred_hosting;
ALTER TABLE runner_pool_org_defaults RENAME COLUMN remote_provided TO pool_id;

UPDATE governance_changes
SET payload = (payload - 'executor') || jsonb_build_object('remote_runner_id', payload->'executor')
WHERE target_kind = 'runner_pool_use_policy' AND state = 'pending' AND payload ? 'executor';
END $$;
