-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run a workload let go of that is still owed its end. The row is written in the transaction that
-- let the run go: the workload's delete, or the record of the launch that replaced it; or, for a run a
-- launch started that the workload never took, just before that run is ended. The API ends the run at
-- once and drops the row. A row left behind (the request went away, a read failed, the process died)
-- is ended by the workload sweep, so a run is never left running with nothing that remembers it.
-- owner, name and generation are the workload's, for the run.kill row.
--
-- after_run_id, when set, is the run that replaces this one and waited in its pool's queue when it was
-- recorded: a start the queue can still refuse. The end waits until that run is running; if it ends
-- before it ran, the workload takes this run back and records the refusal, so a queued spec the start
-- refuses never takes a working service down. Unset (a run that was removed) ends this one at once.
CREATE TABLE IF NOT EXISTS workload_run_ends (
    run_id       uuid PRIMARY KEY REFERENCES agent_runs (id) ON DELETE CASCADE,
    owner        text NOT NULL,
    name         text NOT NULL,
    generation   bigint NOT NULL,
    reason       text NOT NULL CHECK (reason IN ('workload_replaced', 'workload_deleted')),
    after_run_id uuid REFERENCES agent_runs (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workload_run_ends_after_idx ON workload_run_ends (after_run_id) WHERE after_run_id IS NOT NULL;
