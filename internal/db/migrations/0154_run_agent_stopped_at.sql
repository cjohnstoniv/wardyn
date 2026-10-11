-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- When a run kept after a control-plane outage had its agent stopped. An outage
-- run keeps its agent running inside its lease and counts against
-- WARDYN_MAX_CONCURRENT_RUNS for it; once its end passes the lease sweep stops
-- the agent but leaves lost_reason = 'outage'. Extending that run moves its end
-- back into the future, and the end alone then read as an agent still running:
-- a phantom slot under the cap and a "holding" row in the fleet capacity view.
-- Recording the stop lets the one holds-a-sandbox rule tell the two apart.
--
-- NULL for every existing row and for a run whose agent has not been stopped;
-- cleared when a revive claims the run (the revive starts the agent again).
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS agent_stopped_at timestamptz;
