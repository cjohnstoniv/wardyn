-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A quarantine proof flagged "asserted" belongs to a runner-asserted subject, one the organisation has no
-- run row for. A quarantine bound to a real run must never carry that flag: the flag is what tells the
-- runner to treat the proof as unverified. NOT VALID enforces the rule on every new or changed row without
-- failing the migration on a row written before it.
ALTER TABLE runner_pending_actions
    DROP CONSTRAINT IF EXISTS runner_pending_actions_subject,
    ADD CONSTRAINT runner_pending_actions_subject CHECK (
        (subject = 'run' AND run_id IS NOT NULL AND asserted_run_id IS NULL
            AND (kind <> 'quarantine_run' OR (proof->>'asserted')::boolean IS NOT TRUE))
        OR (subject = 'asserted' AND run_id IS NULL AND asserted_run_id IS NOT NULL AND binding_version = 1
            AND kind IN ('kill', 'stop_proxy', 'quarantine_run') AND execution_generation IS NULL
            AND (kind <> 'quarantine_run' OR (proof->>'asserted')::boolean IS TRUE))
    ) NOT VALID;
