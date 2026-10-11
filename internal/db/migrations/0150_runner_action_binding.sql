-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner pending action binds the exact org identity it was queued for: the
-- runner owner, the run's execution generation (NULL on a bound row: no
-- execution had been issued) and, for stop_proxy, the loss it contains. These
-- columns are written once at queue time; only the receipt columns change after.
-- queue_seq is the delivery order: queued_at can tie within a microsecond.
-- binding_version 0 is a row queued before this migration. It carries no owner,
-- generation or loss authority: it still fences its run, is never delivered and
-- cannot be settled by an action_result.
ALTER TABLE runner_pending_actions
    ADD COLUMN IF NOT EXISTS binding_version      SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS owner                TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS execution_generation UUID,
    ADD COLUMN IF NOT EXISTS containment_lost_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS proof                JSONB,
    ADD COLUMN IF NOT EXISTS queue_seq            BIGINT GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN IF NOT EXISTS settled_at           TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS settlement           TEXT NOT NULL DEFAULT '';

-- settled_at/settlement end an obligation without rewriting its receipt. The
-- only settlement so far is 'revoked': the run ended KILLED in the same
-- transaction that revoked its runner.
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
    ),
    DROP CONSTRAINT IF EXISTS runner_pending_actions_settlement,
    ADD CONSTRAINT runner_pending_actions_settlement CHECK (
        settlement IN ('', 'revoked') AND (settled_at IS NULL) = (settlement = '')
        AND (settlement = '' OR binding_version = 1)
    );

-- The fence reads open and deferred rows nothing has settled: a deferred
-- receipt is final for its action but leaves the obligation unresolved.
DROP INDEX IF EXISTS runner_pending_actions_one_open;
CREATE UNIQUE INDEX IF NOT EXISTS runner_pending_actions_one_open_unsettled
    ON runner_pending_actions (run_id, kind) WHERE applied_at IS NULL AND settled_at IS NULL;
CREATE INDEX IF NOT EXISTS runner_pending_actions_runner_page
    ON runner_pending_actions (runner_id, queue_seq) WHERE applied_at IS NULL AND settled_at IS NULL AND binding_version = 1;
CREATE INDEX IF NOT EXISTS runner_pending_actions_run_unsettled
    ON runner_pending_actions (run_id) WHERE (applied_at IS NULL OR outcome = 'deferred') AND settled_at IS NULL;

-- The binding columns never change after insert; a receipt and a settlement
-- are each written once.
-- The store is the only writer; this refuses any other UPDATE at the database.
CREATE OR REPLACE FUNCTION runner_pending_actions_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.id, NEW.runner_id, NEW.run_id, NEW.kind, NEW.ref, NEW.queued_at, NEW.binding_version, NEW.owner,
            NEW.execution_generation, NEW.containment_lost_at, NEW.proof::text, NEW.queue_seq)
        IS DISTINCT FROM (OLD.id, OLD.runner_id, OLD.run_id, OLD.kind, OLD.ref, OLD.queued_at, OLD.binding_version,
            OLD.owner, OLD.execution_generation, OLD.containment_lost_at, OLD.proof::text, OLD.queue_seq)
        OR (OLD.applied_at IS NOT NULL AND (NEW.applied_at, NEW.outcome, NEW.observed_at)
            IS DISTINCT FROM (OLD.applied_at, OLD.outcome, OLD.observed_at))
        OR (OLD.settled_at IS NOT NULL AND (NEW.settled_at, NEW.settlement)
            IS DISTINCT FROM (OLD.settled_at, OLD.settlement)) THEN
        RAISE EXCEPTION 'runner_pending_actions: binding, receipt and settlement columns are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS runner_pending_actions_immutable ON runner_pending_actions;
CREATE TRIGGER runner_pending_actions_immutable
    BEFORE UPDATE ON runner_pending_actions
    FOR EACH ROW EXECUTE FUNCTION runner_pending_actions_immutable();
