-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Generation identity belongs to the org. Exit receipts survive audit retention
-- and cannot be overwritten by a later incarnation of the same sandbox.
CREATE TABLE IF NOT EXISTS runner_executions (
    run_id UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id UUID NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    generation UUID NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('exec', 'start')),
    exec_id TEXT NOT NULL DEFAULT '',
    UNIQUE (run_id, generation)
);
CREATE TABLE IF NOT EXISTS runner_execution_exits (
    run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    generation UUID NOT NULL,
    runner_id UUID NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    payload JSONB NOT NULL,
    payload_hash TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    accepted_at TIMESTAMPTZ,
    tail_complete BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, generation)
);
CREATE INDEX IF NOT EXISTS runner_executions_runner_idx ON runner_executions(runner_id);
CREATE INDEX IF NOT EXISTS runner_execution_exits_runner_idx ON runner_execution_exits(runner_id);
