-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The organisation's side of a Kubernetes runner's node-loss containment.
--
-- run_containment_pending is the last containment-pending report a runner sent for a run: the runner has
-- deleted the run's pods but holds no proof that the processes stopped. The row is removed only when the
-- runner reports the run's proxy stopped, which it does only on completion evidence. name_reused and
-- no_causal_association only ever go from false to true. tuple is what a node record must name.
CREATE TABLE IF NOT EXISTS run_containment_pending (
    run_id                UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id             UUID NOT NULL,
    kind                  TEXT NOT NULL CHECK (kind IN ('agent_only', 'pending_delete')),
    since                 TIMESTAMPTZ NOT NULL,
    name_reused           BOOLEAN NOT NULL DEFAULT false,
    no_causal_association BOOLEAN NOT NULL DEFAULT false,
    tuple                 JSONB,
    observed_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- run_node_records is a security operator's "node powered off" or "no prior holder" record, sent to the
-- runner on a stop_proxy action, and what the runner answered. The people who made it are on the audit
-- rows, not here.
CREATE TABLE IF NOT EXISTS run_node_records (
    id          UUID PRIMARY KEY,
    run_id      UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id   UUID NOT NULL,
    action_id   UUID NOT NULL UNIQUE,
    attestation TEXT NOT NULL CHECK (attestation IN ('node_powered_off', 'no_prior_holder')),
    source      TEXT NOT NULL CHECK (source IN ('provider_console', 'management_controller', 'other')),
    source_id   TEXT NOT NULL CHECK (source_id <> ''),
    state       TEXT NOT NULL DEFAULT 'sent' CHECK (state IN ('sent', 'accepted', 'refused')),
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    decided_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS run_node_records_run ON run_node_records (run_id, created_at);

-- A stop_proxy that carries a node record is not bound to an outage loss: it has no containment_lost_at
-- and carries the record as its proof. Every other stop_proxy keeps 0175's binding, unchanged.
-- On a live upgrade the re-check holds an ACCESS EXCLUSIVE lock on runner_pending_actions while it scans the table.
ALTER TABLE runner_pending_actions
    DROP CONSTRAINT IF EXISTS runner_pending_actions_binding,
    ADD CONSTRAINT runner_pending_actions_binding CHECK (
        (binding_version = 0 AND subject = 'run' AND owner = '' AND execution_generation IS NULL
            AND containment_lost_at IS NULL AND proof IS NULL AND kind IN ('kill', 'end', 'stop_proxy'))
        OR (binding_version = 1 AND ref <> '' AND obligation_id IS NOT NULL
            AND (subject = 'run') = (owner <> '')
            AND (kind = 'stop_proxy' AND proof IS NULL) = (containment_lost_at IS NOT NULL)
            AND (kind IN ('recover_mutation', 'recover_containment', 'quarantine_run', 'retire_run')
                    OR (kind = 'stop_proxy' AND containment_lost_at IS NULL))
                = (proof IS NOT NULL AND jsonb_typeof(proof) = 'object')
            AND (kind <> 'stop_proxy' OR proof IS NULL OR subject = 'run')
            AND ((applied_at IS NULL AND outcome = '' AND observed_at IS NULL)
                OR (applied_at IS NOT NULL AND outcome IN ('success', 'deferred', 'refused') AND observed_at IS NOT NULL)))
    );
