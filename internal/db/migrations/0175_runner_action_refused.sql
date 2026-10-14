-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner answers a pending action it can never apply (its ref is not this
-- runner's, or it names a container the runner cannot prove it created) with
-- the terminal outcome 'refused', so the action closes and the runner's
-- session comes up instead of failing on every reconnect. Only the receipt's
-- outcome list widens; the rest of the binding is 0155's, unchanged.
ALTER TABLE runner_pending_actions
    DROP CONSTRAINT IF EXISTS runner_pending_actions_binding,
    ADD CONSTRAINT runner_pending_actions_binding CHECK (
        (binding_version = 0 AND subject = 'run' AND owner = '' AND execution_generation IS NULL
            AND containment_lost_at IS NULL AND proof IS NULL AND kind IN ('kill', 'end', 'stop_proxy'))
        OR (binding_version = 1 AND ref <> '' AND obligation_id IS NOT NULL
            AND (subject = 'run') = (owner <> '')
            AND (kind = 'stop_proxy') = (containment_lost_at IS NOT NULL)
            AND (kind IN ('recover_mutation', 'recover_containment', 'quarantine_run', 'retire_run'))
                = (proof IS NOT NULL AND jsonb_typeof(proof) = 'object')
            AND ((applied_at IS NULL AND outcome = '' AND observed_at IS NULL)
                OR (applied_at IS NOT NULL AND outcome IN ('success', 'deferred', 'refused') AND observed_at IS NOT NULL)))
    );
