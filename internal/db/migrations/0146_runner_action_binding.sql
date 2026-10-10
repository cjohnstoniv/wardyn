-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner pending action binds the exact org identity it was queued for: the
-- runner owner, the run's execution generation (NULL on a bound row: no
-- execution had been issued) and, for stop_proxy, the loss it contains. These
-- columns are written once at queue time; only the receipt columns change after.
-- binding_version 0 is a row queued before this migration. It carries no owner,
-- generation or loss authority: it still fences its run, is never delivered and
-- cannot be settled by an action_result.
ALTER TABLE runner_pending_actions
    ADD COLUMN IF NOT EXISTS binding_version      SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS owner                TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_generation UUID,
    ADD COLUMN IF NOT EXISTS containment_lost_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS proof                JSONB;

-- proof is the closed disposition metadata of the kinds that need one: the
-- mutation failure marker (recover_*), the quarantine or the retirement receipt.
ALTER TABLE runner_pending_actions
    DROP CONSTRAINT IF EXISTS runner_pending_actions_kind_check,
    ADD CONSTRAINT runner_pending_actions_kind_check
        CHECK (kind IN ('kill', 'end', 'stop_proxy', 'recover_mutation', 'recover_containment', 'quarantine_run', 'retire_run')),
    DROP CONSTRAINT IF EXISTS runner_pending_actions_binding,
    ADD CONSTRAINT runner_pending_actions_binding CHECK (
        (binding_version = 0 AND owner = '' AND execution_generation IS NULL AND containment_lost_at IS NULL
            AND proof IS NULL AND kind IN ('kill', 'end', 'stop_proxy'))
        OR (binding_version = 1 AND owner <> '' AND ref <> ''
            AND (kind = 'stop_proxy') = (containment_lost_at IS NOT NULL)
            AND (kind IN ('recover_mutation', 'recover_containment', 'quarantine_run', 'retire_run'))
                = (proof IS NOT NULL AND jsonb_typeof(proof) = 'object')
            AND ((applied_at IS NULL AND outcome = '' AND observed_at IS NULL)
                OR (applied_at IS NOT NULL AND outcome IN ('success', 'deferred') AND observed_at IS NOT NULL)))
    );

-- The fence reads open and deferred rows: a deferred receipt is final for its
-- action but leaves the obligation unresolved.
CREATE INDEX IF NOT EXISTS runner_pending_actions_run_unsettled
    ON runner_pending_actions (run_id) WHERE applied_at IS NULL OR outcome = 'deferred';
