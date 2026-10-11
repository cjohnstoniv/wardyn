-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner can report a local run the organisation has no row for (an erased
-- or restored-over run). Such an asserted subject is contained, never trusted:
-- a kill and then a quarantine_run bound to the runner-asserted run and ref,
-- with no owner, no execution generation and no FK to agent_runs. Its proof
-- carries the runner-asserted generation flagged "asserted": true.
ALTER TABLE runner_pending_actions
    ADD COLUMN IF NOT EXISTS subject         TEXT NOT NULL DEFAULT 'run',
    ADD COLUMN IF NOT EXISTS asserted_run_id UUID,
    ADD COLUMN IF NOT EXISTS obligation_id   UUID,
    ADD COLUMN IF NOT EXISTS supersedes      UUID REFERENCES runner_pending_actions(id),
    ADD COLUMN IF NOT EXISTS superseded      BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS settled_by      UUID REFERENCES runner_pending_actions(id);
ALTER TABLE runner_pending_actions ALTER COLUMN run_id DROP NOT NULL;

-- The obligation is the chain a deferred action and its successors share: a
-- root is its own obligation, so the insert trigger fills it when absent.
UPDATE runner_pending_actions SET obligation_id = id WHERE obligation_id IS NULL AND binding_version = 1;

CREATE OR REPLACE FUNCTION runner_pending_actions_obligation() RETURNS trigger AS $$
BEGIN
    IF NEW.binding_version = 1 AND NEW.obligation_id IS NULL THEN
        NEW.obligation_id := NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS runner_pending_actions_obligation ON runner_pending_actions;
CREATE TRIGGER runner_pending_actions_obligation
    BEFORE INSERT ON runner_pending_actions
    FOR EACH ROW EXECUTE FUNCTION runner_pending_actions_obligation();

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
                OR (applied_at IS NOT NULL AND outcome IN ('success', 'deferred') AND observed_at IS NOT NULL)))
    ),
    DROP CONSTRAINT IF EXISTS runner_pending_actions_subject,
    ADD CONSTRAINT runner_pending_actions_subject CHECK (
        (subject = 'run' AND run_id IS NOT NULL AND asserted_run_id IS NULL)
        OR (subject = 'asserted' AND run_id IS NULL AND asserted_run_id IS NOT NULL AND binding_version = 1
            AND kind IN ('kill', 'stop_proxy', 'quarantine_run') AND execution_generation IS NULL
            AND (kind <> 'quarantine_run' OR (proof->>'asserted')::boolean IS TRUE))
    ),
    DROP CONSTRAINT IF EXISTS runner_pending_actions_chain,
    ADD CONSTRAINT runner_pending_actions_chain CHECK (
        (NOT superseded OR outcome = 'deferred') AND (supersedes IS NULL OR supersedes <> id)
    ),
    DROP CONSTRAINT IF EXISTS runner_pending_actions_settlement,
    ADD CONSTRAINT runner_pending_actions_settlement CHECK (
        settlement IN ('', 'revoked', 'chain') AND (settled_at IS NULL) = (settlement = '')
        AND (settlement = '' OR binding_version = 1)
        AND (settlement = 'chain') = (settled_by IS NOT NULL)
    );

-- Re-asserting a subject reuses its rows: one chain root per (runner, run,
-- ref, kind), settled or not. A successor continues its predecessor's chain.
CREATE UNIQUE INDEX IF NOT EXISTS runner_pending_actions_asserted_root
    ON runner_pending_actions (runner_id, asserted_run_id, ref, kind) WHERE subject = 'asserted' AND supersedes IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS runner_pending_actions_one_successor
    ON runner_pending_actions (supersedes) WHERE supersedes IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS runner_pending_actions_asserted_tip
    ON runner_pending_actions (runner_id, asserted_run_id, kind) WHERE subject = 'asserted' AND settled_at IS NULL AND NOT superseded;

-- The subject, chain links and binding never change after insert; superseded
-- only goes false to true; a receipt and a settlement are each written once.
CREATE OR REPLACE FUNCTION runner_pending_actions_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.id, NEW.runner_id, NEW.run_id, NEW.kind, NEW.ref, NEW.queued_at, NEW.binding_version, NEW.owner,
            NEW.execution_generation, NEW.containment_lost_at, NEW.proof::text, NEW.queue_seq,
            NEW.subject, NEW.asserted_run_id, NEW.obligation_id, NEW.supersedes)
        IS DISTINCT FROM (OLD.id, OLD.runner_id, OLD.run_id, OLD.kind, OLD.ref, OLD.queued_at, OLD.binding_version,
            OLD.owner, OLD.execution_generation, OLD.containment_lost_at, OLD.proof::text, OLD.queue_seq,
            OLD.subject, OLD.asserted_run_id, OLD.obligation_id, OLD.supersedes)
        OR (OLD.superseded AND NOT NEW.superseded)
        OR (OLD.applied_at IS NOT NULL AND (NEW.applied_at, NEW.outcome, NEW.observed_at)
            IS DISTINCT FROM (OLD.applied_at, OLD.outcome, OLD.observed_at))
        OR (OLD.settled_at IS NOT NULL AND (NEW.settled_at, NEW.settlement, NEW.settled_by)
            IS DISTINCT FROM (OLD.settled_at, OLD.settlement, OLD.settled_by)) THEN
        RAISE EXCEPTION 'runner_pending_actions: binding, receipt and settlement columns are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
