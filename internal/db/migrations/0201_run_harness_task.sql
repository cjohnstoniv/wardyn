-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A harness's own task in a background run with several agents (tools[].task,
-- WARDYN_MULTI_AGENT_TASKS): every harness but the run's agent, whose task is
-- agent_runs.task, carries one. Empty for every other row. It is the person's
-- text, so person erasure blanks it as it blanks agent_runs.task; split-role
-- installs that turn several agents on grant the app role UPDATE on
-- run_harnesses for that.
ALTER TABLE run_harnesses
    ADD COLUMN IF NOT EXISTS task TEXT NOT NULL DEFAULT '';
