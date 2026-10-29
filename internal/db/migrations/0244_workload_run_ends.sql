-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run a workload let go of that is still owed its end. The row is written in the transaction that
-- let the run go: the workload's delete, or the record of the launch that replaced it; or, for a run a
-- launch started that the workload never took, just before that run is ended. The API ends the run at
-- once and drops the row. A row left behind (the request went away, a read failed, the process died)
-- is ended by the workload sweep, so a run is never left running with nothing that remembers it.
-- owner, name and generation are the workload's, for the run.kill row.
CREATE TABLE IF NOT EXISTS workload_run_ends (
    run_id     uuid PRIMARY KEY REFERENCES agent_runs (id) ON DELETE CASCADE,
    owner      text NOT NULL,
    name       text NOT NULL,
    generation bigint NOT NULL,
    reason     text NOT NULL CHECK (reason IN ('workload_replaced', 'workload_deleted')),
    created_at timestamptz NOT NULL DEFAULT now()
);
